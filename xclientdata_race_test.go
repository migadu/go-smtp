package smtp_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// signallingXCLIENTSession accepts every XCLIENT. Its hook tells the test that the command is
// being handled, then waits without any synchronization so that a concurrent XCLIENTData()
// call runs before go-smtp records the attributes.
type signallingXCLIENTSession struct {
	inHook chan struct{}
}

func (*signallingXCLIENTSession) Reset()                                                {}
func (*signallingXCLIENTSession) Logout() error                                         { return nil }
func (*signallingXCLIENTSession) Mail(context.Context, string, *smtp.MailOptions) error { return nil }
func (*signallingXCLIENTSession) Rcpt(context.Context, string, *smtp.RcptOptions) error { return nil }
func (*signallingXCLIENTSession) Data(context.Context, io.Reader) error                 { return nil }

func (s *signallingXCLIENTSession) XCLIENT(smtp.Session, map[string]string) error {
	if s.inHook != nil {
		close(s.inHook)
		// A sleep, not a channel or lock: it must not order the concurrent read
		// before the write that follows the hook.
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// XCLIENTData may be called from any goroutine, including while the connection is handling an
// XCLIENT command, so recording the attributes must hold the lock XCLIENTData reads them under
// (issue #41). Run with -race. The reader calls XCLIENTData() while the hook runs; nothing it
// does afterwards synchronizes with the connection goroutine, so an unlocked write after the
// hook is reported as a race with that read.
func TestXCLIENTDataConcurrentWithXCLIENT(t *testing.T) {
	conns := make(chan *smtp.Conn, 1)
	sessions := make(chan *signallingXCLIENTSession, 8)
	srv := smtp.NewServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
		select {
		case conns <- c:
		default:
		}
		s := <-sessions
		return s, nil
	}))
	srv.Domain = "localhost"
	srv.EnableXCLIENT = true
	if err := srv.AddXCLIENTTrustedNetwork("127.0.0.0/8"); err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	scanner := bufio.NewScanner(c)
	readResponseLine(t, scanner) // greeting

	ehlo := func() {
		t.Helper()
		io.WriteString(c, "EHLO localhost\r\n")
		for {
			if line := readResponseLine(t, scanner); strings.HasPrefix(line, "250 ") {
				return
			} else if !strings.HasPrefix(line, "250-") {
				t.Fatalf("EHLO: %s", line)
			}
		}
	}

	// The first XCLIENT creates xclientData, the second adds to it. Each EHLO starts the session
	// whose hook handles the XCLIENT that follows it.
	var conn *smtp.Conn
	for _, arg := range []string{"ADDR=192.0.2.1", "ADDR=192.0.2.2 LOGIN=user@example.com"} {
		s := &signallingXCLIENTSession{inHook: make(chan struct{})}
		sessions <- s
		ehlo()
		if conn == nil {
			conn = <-conns
		}

		read := make(chan struct{})
		go func() {
			defer close(read)
			<-s.inHook
			conn.XCLIENTData()
		}()

		io.WriteString(c, "XCLIENT "+arg+"\r\n")
		if line := readResponseLine(t, scanner); !strings.HasPrefix(line, "220 ") {
			t.Fatalf("XCLIENT %s: %s", arg, line)
		}
		<-read
	}
}
