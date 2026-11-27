package smtp_test

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

type bdatLifecycleSession struct {
	ctxSession
	conn          *smtp.Conn
	closeFromData bool
	panicInLogout bool
	interrupted   chan struct{}
	allowReturn   chan struct{}
	loggedOut     chan struct{}
}

func (s *bdatLifecycleSession) Data(_ context.Context, r io.Reader) error {
	var err error
	if s.closeFromData {
		if _, err = io.ReadFull(r, make([]byte, 1)); err == nil {
			err = s.conn.Close()
		}
	} else {
		_, err = io.Copy(io.Discard, r)
	}
	close(s.interrupted)
	<-s.allowReturn
	return err
}

func (s *bdatLifecycleSession) Logout() error {
	defer close(s.loggedOut)
	if s.panicInLogout {
		panic("backend Logout panic")
	}
	return nil
}

type bdatPanicLog chan struct{}

func (l bdatPanicLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "backend Logout panic") {
		close(l)
	}
	return len(p), nil
}

func TestServer_BDATLogoutWaitsForData(t *testing.T) {
	for _, how := range []string{"STARTTLS", "client disconnect", "Server.Close", "Close from Data", "panicking Logout after QUIT"} {
		t.Run(how, func(t *testing.T) {
			session := &bdatLifecycleSession{
				closeFromData: how == "Close from Data",
				panicInLogout: how == "panicking Logout after QUIT",
				interrupted:   make(chan struct{}),
				allowReturn:   make(chan struct{}),
				loggedOut:     make(chan struct{}),
			}
			panicLogged := make(bdatPanicLog)
			_, server, conn, scanner, _ := testServerEhlo(t, withTLS, func(s *smtp.Server) {
				if session.panicInLogout {
					s.ErrorLog = log.New(panicLogged, "", 0)
				}
				s.Backend = smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
					session.conn = c
					return session, nil
				})
			})
			defer server.Close()
			defer conn.Close()
			var release sync.Once
			allowReturn := func() { release.Do(func() { close(session.allowReturn) }) }
			defer allowReturn()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			expect := func(prefix string) {
				t.Helper()
				if reply := readReply(t, scanner); !strings.HasPrefix(reply, prefix) {
					t.Fatalf("reply = %q, want %s", reply, prefix)
				}
			}
			io.WriteString(conn, "MAIL FROM:<sender@example.com>\r\n")
			expect("250 ")
			io.WriteString(conn, "RCPT TO:<rcpt@example.com>\r\n")
			expect("250 ")
			io.WriteString(conn, "BDAT 1\r\nx")
			if !session.closeFromData {
				expect("250 ")
			}
			switch how {
			case "STARTTLS":
				io.WriteString(conn, "STARTTLS\r\n")
				expect("220 ")
				if err := tls.Client(conn, &tls.Config{InsecureSkipVerify: true}).Handshake(); err != nil {
					t.Fatal(err)
				}
			case "client disconnect":
				conn.Close()
			case "panicking Logout after QUIT":
				io.WriteString(conn, "QUIT\r\n")
				expect("221 ")
			case "Server.Close":
				done := make(chan error, 1)
				go func() { done <- server.Close() }()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("Server.Close waited for backend Data")
				}
			}
			select {
			case <-session.interrupted:
			case <-time.After(time.Second):
				t.Fatal("Data did not observe the abort or return from Conn.Close")
			}
			select {
			case <-session.loggedOut:
				t.Fatal("Logout ran while Data was still running")
			case <-time.After(50 * time.Millisecond):
			}
			allowReturn()
			select {
			case <-session.loggedOut:
			case <-time.After(time.Second):
				t.Fatal("Logout did not run after Data returned")
			}
			if session.panicInLogout {
				select {
				case <-panicLogged:
				case <-time.After(time.Second):
					t.Fatal("Logout panic was not logged")
				}
			}
		})
	}
}
