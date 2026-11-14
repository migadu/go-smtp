package smtp_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// contentSession reports what the backend read from the message content: the
// number of bytes and the error the reader ended with.
type contentSession struct {
	ctxSession
	got chan contentResult
}

type contentResult struct {
	n   int
	err error
}

func (s contentSession) Data(ctx context.Context, r io.Reader) error {
	b, err := io.ReadAll(r)
	s.got <- contentResult{len(b), err}
	return err
}

type contentLMTPSession struct{ contentSession }

func (s contentLMTPSession) LMTPData(ctx context.Context, r io.Reader, _ smtp.StatusCollector) error {
	return s.Data(ctx, r)
}

// transferMode is one way of sending message content.
type transferMode struct {
	name string
	lmtp bool
	bdat bool
}

var transferModes = []transferMode{
	{name: "DATA"},
	{name: "LMTP DATA", lmtp: true},
	{name: "BDAT", bdat: true},
}

type transferTest struct {
	t       *testing.T
	mode    transferMode
	conn    net.Conn
	scanner *bufio.Scanner
	smtpc   *smtp.Conn
	got     chan contentResult
}

// startTransfer opens a session and sends the envelope (one recipient), so the
// next step is the content command.
func startTransfer(t *testing.T, mode transferMode, configure func(*smtp.Server)) *transferTest {
	t.Helper()
	got := make(chan contentResult, 1)
	conns := make(chan *smtp.Conn, 1)
	_, server, conn, scanner := testServer(t, func(s *smtp.Server) {
		s.LMTP = mode.lmtp
		s.Backend = smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
			conns <- c
			sess := contentSession{got: got}
			if mode.lmtp {
				return contentLMTPSession{sess}, nil
			}
			return sess, nil
		})
		if configure != nil {
			configure(s)
		}
	})
	t.Cleanup(func() {
		conn.Close()
		server.Close()
	})
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	readGreetingReply(t, scanner, "220")
	hello := "EHLO"
	if mode.lmtp {
		hello = "LHLO"
	}
	io.WriteString(conn, hello+" client.example.org\r\n")
	readGreetingReply(t, scanner, "250")
	io.WriteString(conn, "MAIL FROM:<sender@example.org>\r\n")
	readGreetingReply(t, scanner, "250")
	io.WriteString(conn, "RCPT TO:<user@example.com>\r\n")
	readGreetingReply(t, scanner, "250")
	return &transferTest{t: t, mode: mode, conn: conn, scanner: scanner, smtpc: <-conns, got: got}
}

// begin sends the content command for a message of size bytes (the size
// matters only to BDAT, which sends the whole message as one LAST chunk).
func (tt *transferTest) begin(size int) {
	tt.t.Helper()
	if tt.mode.bdat {
		fmt.Fprintf(tt.conn, "BDAT %d LAST\r\n", size)
		return
	}
	io.WriteString(tt.conn, "DATA\r\n")
	readGreetingReply(tt.t, tt.scanner, "354")
}

// end sends the DATA end-of-data marker; BDAT needs none.
func (tt *transferTest) end() {
	if !tt.mode.bdat {
		io.WriteString(tt.conn, ".\r\n")
	}
}

// reply returns the next reply line: the content command's (single
// recipient, so one line in LMTP too).
func (tt *transferTest) reply() string {
	tt.t.Helper()
	if !tt.scanner.Scan() {
		tt.t.Fatalf("no reply to the message content: %v", tt.scanner.Err())
	}
	return tt.scanner.Text()
}

// failed reports whether the backend's read ended in the error a stalled
// transfer should produce: the timeout itself for DATA, whose reader the
// backend reads directly, and any error for BDAT, whose pipe the server
// aborts (ErrDataReset).
func (tt *transferTest) failed(r contentResult) bool {
	if tt.mode.bdat {
		return r.err != nil
	}
	return isTimeout(r.err)
}

func (tt *transferTest) result() contentResult {
	tt.t.Helper()
	select {
	case r := <-tt.got:
		return r
	case <-time.After(5 * time.Second):
		tt.t.Fatal("the backend did not finish reading the content")
		return contentResult{}
	}
}

const trickleLine = "0123456789012345678901234567890123456789\r\n"

// trickle sends lines of content, one every interval.
func trickle(conn net.Conn, lines int, interval time.Duration) {
	for i := 0; i < lines; i++ {
		if _, err := io.WriteString(conn, trickleLine); err != nil {
			return
		}
		time.Sleep(interval)
	}
}

// RFC 5321 section 4.5.3.2 sets "a timer ... for each buffer of the data
// transfer", so "the overall timeout is inherently proportional to the size
// of the message". ReadTimeout must therefore bound each wait for more
// content, not the whole transfer: content that keeps arriving for three
// times ReadTimeout is delivered.
func TestReadTimeoutAppliesToEachContentRead(t *testing.T) {
	const (
		readTimeout = 500 * time.Millisecond
		lines       = 30
		interval    = 50 * time.Millisecond // 30 x 50ms = 3 x readTimeout
	)
	for _, mode := range transferModes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			tt := startTransfer(t, mode, func(s *smtp.Server) { s.ReadTimeout = readTimeout })
			tt.begin(lines * len(trickleLine))
			trickle(tt.conn, lines, interval)
			tt.end()

			if r := tt.result(); r.err != nil || r.n != lines*len(trickleLine) {
				t.Errorf("backend read %d of %d bytes, error %v", r.n, lines*len(trickleLine), r.err)
			}
			if reply := tt.reply(); !strings.HasPrefix(reply, "250 ") {
				t.Errorf("content still arriving after ReadTimeout was refused: %q", reply)
			}
		})
	}
}

// A client that stops sending in the middle of the content is cut off. The
// rest of its input can no longer be told apart from commands, so after the
// reply the server closes the connection (RFC 5321 section 3.8 allows closing
// "after a timeout ... occurs waiting for the client to send a command or
// data") instead of reading the remaining content as commands. OnTimeout
// counts the disconnect once.
func TestContentReadTimeoutClosesConnection(t *testing.T) {
	for _, mode := range transferModes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			var timeouts atomic.Int32
			tt := startTransfer(t, mode, func(s *smtp.Server) {
				s.ReadTimeout = 200 * time.Millisecond
				s.OnTimeout = func() { timeouts.Add(1) }
			})
			tt.begin(10 * len(trickleLine))
			io.WriteString(tt.conn, trickleLine)

			if r := tt.result(); !tt.failed(r) {
				t.Errorf("backend read error = %v, want the transfer to fail", r.err)
			}
			if reply := tt.reply(); strings.HasPrefix(reply, "2") {
				t.Errorf("stalled transfer answered %q", reply)
			}
			// What the client sends next is the rest of its message, not a command.
			io.WriteString(tt.conn, "NOOP\r\n")
			if tt.scanner.Scan() {
				t.Errorf("connection kept open after the content timed out: read %q", tt.scanner.Text())
			}
			deadline := time.Now().Add(2 * time.Second)
			for timeouts.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := timeouts.Load(); n != 1 {
				t.Errorf("OnTimeout called %d times, want 1", n)
			}
		})
	}
}

// A BDAT chunk that ends early because the client closed the connection is
// not the complete chunk: the backend must not receive it as the end of the
// message.
func TestBDATChunkCutShortIsNotDelivered(t *testing.T) {
	tt := startTransfer(t, transferMode{name: "BDAT", bdat: true}, nil)
	tt.begin(10 * len(trickleLine))
	io.WriteString(tt.conn, trickleLine)
	tt.conn.(*net.TCPConn).CloseWrite()

	if r := tt.result(); r.err == nil {
		t.Errorf("backend received a %d-byte BDAT LAST chunk of 420 bytes as a complete message", r.n)
	}
}

// The session deadline caps every content read, including when it was set
// while the server was already waiting for the content command.
func TestSessionDeadlineCapsContentRead(t *testing.T) {
	for _, mode := range transferModes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			tt := startTransfer(t, mode, func(s *smtp.Server) { s.ReadTimeout = time.Minute })
			tt.smtpc.SetSessionDeadline(time.Now().Add(300 * time.Millisecond))
			tt.begin(40 * len(trickleLine))
			go trickle(tt.conn, 40, 50*time.Millisecond)

			start := time.Now()
			if r := tt.result(); !tt.failed(r) {
				t.Errorf("backend read error = %v, want the transfer to fail", r.err)
			}
			if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
				t.Errorf("transfer ran %v past a session deadline 300ms away", elapsed)
			}
		})
	}
}

// DataTimeout bounds one transfer however steadily the client sends, and
// ends the connection as any other content timeout does.
func TestDataTimeoutBoundsTransfer(t *testing.T) {
	for _, mode := range transferModes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			var timeouts atomic.Int32
			tt := startTransfer(t, mode, func(s *smtp.Server) {
				s.ReadTimeout = time.Minute
				s.DataTimeout = 300 * time.Millisecond
				s.OnTimeout = func() { timeouts.Add(1) }
			})
			tt.begin(40 * len(trickleLine))
			go trickle(tt.conn, 40, 50*time.Millisecond)

			start := time.Now()
			if r := tt.result(); !tt.failed(r) {
				t.Errorf("backend read error = %v, want the transfer to fail", r.err)
			}
			if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
				t.Errorf("transfer ran %v with a DataTimeout of 300ms", elapsed)
			}
			if reply := tt.reply(); strings.HasPrefix(reply, "2") {
				t.Errorf("transfer cut off by DataTimeout answered %q", reply)
			}
			if tt.scanner.Scan() {
				t.Errorf("connection kept open after DataTimeout: read %q", tt.scanner.Text())
			}
			deadline := time.Now().Add(2 * time.Second)
			for timeouts.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := timeouts.Load(); n != 1 {
				t.Errorf("OnTimeout called %d times, want 1", n)
			}
		})
	}
}

// The deadline DataTimeout sets ends with its transfer, also when no
// ReadTimeout replaces it: the next command may arrive later than that.
func TestDataTimeoutEndsWithTransfer(t *testing.T) {
	for _, mode := range transferModes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			tt := startTransfer(t, mode, func(s *smtp.Server) {
				s.ReadTimeout = 0
				s.DataTimeout = 200 * time.Millisecond
			})
			tt.begin(len(trickleLine))
			io.WriteString(tt.conn, trickleLine)
			tt.end()
			if r := tt.result(); r.err != nil {
				t.Fatalf("backend read error = %v", r.err)
			}
			if reply := tt.reply(); !strings.HasPrefix(reply, "250 ") {
				t.Fatalf("transfer answered %q", reply)
			}
			time.Sleep(300 * time.Millisecond)
			io.WriteString(tt.conn, "NOOP\r\n")
			readGreetingReply(t, tt.scanner, "250")
		})
	}
}

func isTimeout(err error) bool {
	for err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
