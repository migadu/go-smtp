package smtp_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

type delayedBDATResultSession struct {
	ctxSession
	calls         atomic.Int32
	firstRead     chan struct{}
	secondRead    chan struct{}
	firstReturn   chan struct{}
	secondReturn  chan struct{}
	firstReturned chan struct{}
	panicFirst    bool
}

func (s *delayedBDATResultSession) Data(_ context.Context, r io.Reader) error {
	call := s.calls.Add(1)
	_, err := io.Copy(io.Discard, r)
	if call == 1 {
		defer close(s.firstReturned)
		close(s.firstRead)
		<-s.firstReturn
		if s.panicFirst {
			panic("aborted transfer panicked")
		}
		return errors.New("aborted transfer: " + err.Error())
	}
	close(s.secondRead)
	<-s.secondReturn
	return err
}

// Reset must wait for an aborted backend call to return. Its result, including
// a recovered panic, belongs to that transfer alone.
func TestServer_BDATAbortedResultDoesNotReachNextTransaction(t *testing.T) {
	for _, lmtp := range []bool{false, true} {
		for _, panicFirst := range []bool{false, true} {
			name := "SMTP"
			if lmtp {
				name = "LMTP"
			}
			if panicFirst {
				name += "/panic"
			}
			t.Run(name, func(t *testing.T) {
				sess := &delayedBDATResultSession{
					firstRead: make(chan struct{}), secondRead: make(chan struct{}),
					firstReturn: make(chan struct{}), secondReturn: make(chan struct{}),
					firstReturned: make(chan struct{}), panicFirst: panicFirst,
				}
				var first, second sync.Once
				t.Cleanup(func() {
					first.Do(func() { close(sess.firstReturn) })
					second.Do(func() { close(sess.secondReturn) })
				})
				_, server, conn, _ := testServer(t, func(s *smtp.Server) {
					s.LMTP = lmtp
					s.Backend = smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) { return sess, nil })
				})
				t.Cleanup(func() { conn.Close(); server.Close() })
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(conn)
				if line := bdatReadReply(t, r); !strings.HasPrefix(line, "220 ") {
					t.Fatalf("greeting: %q", line)
				}
				hello := "EHLO"
				if lmtp {
					hello = "LHLO"
				}
				io.WriteString(conn, hello+" client.example.org\r\n")
				for !strings.HasPrefix(bdatReadReply(t, r), "250 ") {
				}
				command := func(cmd, prefix string) {
					t.Helper()
					io.WriteString(conn, cmd)
					if line := bdatReadReply(t, r); !strings.HasPrefix(line, prefix) {
						t.Fatalf("%q: %q, want %q", cmd, line, prefix)
					}
				}
				envelope := func() {
					command("MAIL FROM:<sender@example.org>\r\n", "250 ")
					command("RCPT TO:<rcpt@example.org>\r\n", "250 ")
				}
				wait := func(ch <-chan struct{}) {
					t.Helper()
					select {
					case <-ch:
					case <-time.After(3 * time.Second):
						t.Fatal("backend did not reach the expected transfer state")
					}
				}
				readAsync := func() <-chan string {
					replies := make(chan string, 1)
					go func() {
						line, _ := r.ReadString('\n')
						replies <- line
					}()
					return replies
				}
				envelope()
				command("BDAT 1\r\na", "250 ")
				io.WriteString(conn, "RSET\r\n")
				wait(sess.firstRead)
				resetReply := readAsync()
				select {
				case line := <-resetReply:
					t.Fatalf("RSET answered before its backend Data returned: %q", line)
				case <-time.After(100 * time.Millisecond):
				}
				first.Do(func() { close(sess.firstReturn) })
				wait(sess.firstReturned)
				if line := <-resetReply; !strings.HasPrefix(line, "250 ") {
					t.Fatalf("RSET: %q, want 250", line)
				}
				envelope()
				io.WriteString(conn, "BDAT 1 LAST\r\nb")
				wait(sess.secondRead)
				replies := readAsync()
				select {
				case line := <-replies:
					t.Fatalf("second transfer answered before its backend returned: %q", line)
				case <-time.After(100 * time.Millisecond):
				}
				second.Do(func() { close(sess.secondReturn) })
				if line := <-replies; !strings.HasPrefix(line, "250 ") {
					t.Fatalf("second transfer: %q, want its own successful result", line)
				}
			})
		}
	}
}
