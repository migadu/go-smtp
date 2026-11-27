package smtp_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// closeIdleBackend's Data reads the whole message, then holds the reply until
// the test releases it: a delivery still in progress.
type closeIdleBackend struct {
	dataStarted chan struct{}
	release     chan struct{}

	// If set, Data stops after the first byte of the message, signals
	// paused and waits for resume, holding the server inside the chunk.
	paused chan struct{}
	resume chan struct{}
}

func newCloseIdleBackend() *closeIdleBackend {
	return &closeIdleBackend{dataStarted: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *closeIdleBackend) awaitData(t *testing.T) {
	t.Helper()
	select {
	case <-b.dataStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("the message never reached Data")
	}
}

func (b *closeIdleBackend) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &closeIdleSession{b}, nil
}

type closeIdleSession struct{ backend *closeIdleBackend }

func (*closeIdleSession) Reset()        {}
func (*closeIdleSession) Logout() error { return nil }
func (*closeIdleSession) Mail(ctx context.Context, from string, opts *smtp.MailOptions) error {
	return nil
}
func (*closeIdleSession) Rcpt(ctx context.Context, to string, opts *smtp.RcptOptions) error {
	return nil
}
func (s *closeIdleSession) Data(ctx context.Context, r io.Reader) error {
	if s.backend.paused != nil {
		if _, err := io.ReadFull(r, make([]byte, 1)); err != nil {
			return err
		}
		s.backend.paused <- struct{}{}
		select {
		case <-s.backend.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	s.backend.dataStarted <- struct{}{}
	select {
	case <-s.backend.release:
	case <-ctx.Done():
	}
	return ctx.Err()
}

type closeIdleClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func startCloseIdleServer(t *testing.T, be smtp.Backend, fn ...serverConfigureFunc) (*smtp.Server, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	s := smtp.NewServer(be)
	s.Domain = "localhost"
	for _, f := range fn {
		f(s)
	}
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	return s, l.Addr().String()
}

func dialCloseIdle(t *testing.T, addr string) *closeIdleClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	// Every expectation here is met in milliseconds; a connection that is
	// still open after this was not closed.
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	c := &closeIdleClient{t: t, conn: conn, br: bufio.NewReader(conn)}
	c.expect("220")
	return c
}

// expect reads reply lines up to the final one, which must carry code.
func (c *closeIdleClient) expect(code string) {
	c.t.Helper()
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("want %s: %v", code, err)
		}
		if strings.HasPrefix(line, code+"-") {
			continue
		}
		if !strings.HasPrefix(line, code+" ") {
			c.t.Fatalf("want %s, got %q", code, strings.TrimSpace(line))
		}
		return
	}
}

func (c *closeIdleClient) cmd(line, code string) {
	c.t.Helper()
	c.write(line + "\r\n")
	c.expect(code)
}

func (c *closeIdleClient) write(input string) {
	c.t.Helper()
	if _, err := io.WriteString(c.conn, input); err != nil {
		c.t.Fatal(err)
	}
}

// expectClosed wants the shutdown reply and then the end of the connection.
func (c *closeIdleClient) expectClosed() {
	c.t.Helper()
	c.expect("421 4.3.2")
	if line, err := c.br.ReadString('\n'); !errors.Is(err, io.EOF) {
		c.t.Fatalf("want the connection closed after 421, got %q, %v", line, err)
	}
}

func TestServerCloseIdle_ClosesConnectionsWaitingForACommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(c *closeIdleClient)
	}{
		{"after the greeting", func(*closeIdleClient) {}},
		{"after EHLO", func(c *closeIdleClient) { c.cmd("EHLO client.example.com", "250") }},
		// Nothing of a transaction is in flight before DATA: the client
		// retries it whole.
		{"inside a transaction", func(c *closeIdleClient) {
			c.cmd("EHLO client.example.com", "250")
			c.cmd("MAIL FROM:<sender@example.com>", "250")
			c.cmd("RCPT TO:<rcpt@example.com>", "250")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, addr := startCloseIdleServer(t, newCloseIdleBackend())
			c := dialCloseIdle(t, addr)
			tc.setup(c)
			s.CloseIdle()
			c.expectClosed()
		})
	}
}

// The read deadline the server arms for each command must not outlast the
// shutdown.
func TestServerCloseIdle_WithReadTimeout(t *testing.T) {
	var timedOut atomic.Bool
	s, addr := startCloseIdleServer(t, newCloseIdleBackend(), func(s *smtp.Server) {
		s.ReadTimeout = time.Minute
		s.OnTimeout = func() { timedOut.Store(true) }
	})
	c := dialCloseIdle(t, addr)
	c.cmd("EHLO client.example.com", "250")
	s.CloseIdle()
	c.expectClosed()
	if timedOut.Load() {
		t.Error("CloseIdle caused an OnTimeout notification")
	}
}

func TestServerCloseIdle_ClosesConnectionsAcceptedLater(t *testing.T) {
	s, addr := startCloseIdleServer(t, newCloseIdleBackend())
	s.CloseIdle()
	dialCloseIdle(t, addr).expectClosed()
}

// DATA can finish receiving the message and finish delivering it before the
// connection is closed at the next command boundary.
func TestServerCloseIdle_LetsDATAFinish(t *testing.T) {
	for _, phase := range []string{"receiving", "delivering"} {
		t.Run(phase, func(t *testing.T) {
			be := newCloseIdleBackend()
			s, addr := startCloseIdleServer(t, be, func(s *smtp.Server) {
				s.DataTimeout = time.Minute
			})
			c := dialCloseIdle(t, addr)
			c.cmd("EHLO client.example.com", "250")
			c.cmd("MAIL FROM:<sender@example.com>", "250")
			c.cmd("RCPT TO:<rcpt@example.com>", "250")
			c.cmd("DATA", "354")
			c.write("Subject: hi\r\n\r\n")
			if phase == "receiving" {
				s.CloseIdle()
			}
			c.write("hello\r\n.\r\n")
			be.awaitData(t)
			if phase == "delivering" {
				s.CloseIdle()
			}

			close(be.release)
			c.expect("250")
			c.expectClosed()
		})
	}
}

// Between the chunks of a BDAT transfer the client is still sending the
// message: the connection is not idle.
func TestServerCloseIdle_LetsBDATTransferFinish(t *testing.T) {
	be := newCloseIdleBackend()
	s, addr := startCloseIdleServer(t, be)
	c := dialCloseIdle(t, addr)
	c.cmd("EHLO client.example.com", "250")
	c.cmd("MAIL FROM:<sender@example.com>", "250")
	c.cmd("RCPT TO:<rcpt@example.com>", "250")
	c.write("BDAT 13\r\nSubject: hi\r\n")
	c.expect("250")

	s.CloseIdle()
	c.write("BDAT 7 LAST\r\n\r\nhello")
	be.awaitData(t)
	close(be.release)
	c.expect("250")
	c.expectClosed()
}

// CloseIdle called while a chunk is still being handed to Data: the server
// returns to its command loop with the transfer still open, and must go on
// reading it.
func TestServerCloseIdle_DuringABDATChunk(t *testing.T) {
	be := newCloseIdleBackend()
	be.paused = make(chan struct{}, 1)
	be.resume = make(chan struct{})
	s, addr := startCloseIdleServer(t, be)
	c := dialCloseIdle(t, addr)
	c.cmd("EHLO client.example.com", "250")
	c.cmd("MAIL FROM:<sender@example.com>", "250")
	c.cmd("RCPT TO:<rcpt@example.com>", "250")
	c.write("BDAT 13\r\nSubject: hi\r\n")
	select {
	case <-be.paused:
	case <-time.After(3 * time.Second):
		t.Fatal("the chunk never reached Data")
	}

	s.CloseIdle()
	close(be.resume)
	c.expect("250")
	c.write("BDAT 7 LAST\r\n\r\nhello")
	be.awaitData(t)
	close(be.release)
	c.expect("250")
	c.expectClosed()
}
