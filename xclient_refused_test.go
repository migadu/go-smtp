package smtp_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// refusingXCLIENTSession refuses an XCLIENT command that carries LOGIN, a check go-smtp does
// not share with the hook.
type refusingXCLIENTSession struct{}

func (refusingXCLIENTSession) Reset()                                                {}
func (refusingXCLIENTSession) Logout() error                                         { return nil }
func (refusingXCLIENTSession) Mail(context.Context, string, *smtp.MailOptions) error { return nil }
func (refusingXCLIENTSession) Rcpt(context.Context, string, *smtp.RcptOptions) error { return nil }
func (refusingXCLIENTSession) Data(context.Context, io.Reader) error                 { return nil }

func (refusingXCLIENTSession) XCLIENT(_ smtp.Session, attrs map[string]string) error {
	if _, ok := attrs["LOGIN"]; ok {
		return errors.New("LOGIN not accepted here")
	}
	return nil
}

// XCLIENTData never returns attributes the server's XCLIENT hook refused, so a backend that
// re-applies them to the next session (as one created by a later EHLO) does not undo the
// refusal. A refused XCLIENT leaves the attributes accepted earlier as they were (issue #39).
func TestXCLIENTRefusedByHookIsNotRecorded(t *testing.T) {
	// Each new session reports the XCLIENT data the connection holds when it is created.
	seen := make(chan map[string]string, 8)
	srv := smtp.NewServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
		seen <- c.XCLIENTData()
		return refusingXCLIENTSession{}, nil
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

	// ehlo starts a new session and returns the XCLIENT data it was created with.
	ehlo := func() map[string]string {
		t.Helper()
		io.WriteString(c, "EHLO localhost\r\n")
		for {
			if line := readResponseLine(t, scanner); strings.HasPrefix(line, "250 ") {
				break
			} else if !strings.HasPrefix(line, "250-") {
				t.Fatalf("EHLO: %s", line)
			}
		}
		select {
		case data := <-seen:
			return data
		case <-time.After(5 * time.Second):
			t.Fatal("EHLO did not create a session")
			return nil
		}
	}
	xclient := func(arg, wantPrefix string) {
		t.Helper()
		io.WriteString(c, "XCLIENT "+arg+"\r\n")
		if line := readResponseLine(t, scanner); !strings.HasPrefix(line, wantPrefix) {
			t.Fatalf("XCLIENT %s: got %q, want %q...", arg, line, wantPrefix)
		}
	}

	ehlo()

	// Refused with nothing accepted before: nothing is recorded.
	xclient("ADDR=198.51.100.7 LOGIN=mallory", "451 ")
	if data := ehlo(); len(data) != 0 {
		t.Fatalf("after a refused XCLIENT, XCLIENTData() = %v, want none", data)
	}

	// Accepted: recorded, and the server greets again.
	xclient("ADDR=192.0.2.1", "220 ")
	want := map[string]string{"ADDR": "192.0.2.1"}
	if data := ehlo(); !reflect.DeepEqual(data, want) {
		t.Fatalf("after an accepted XCLIENT, XCLIENTData() = %v, want %v", data, want)
	}

	// Refused after an accepted one: the accepted attributes stay, the refused ones are not added.
	xclient("ADDR=198.51.100.7 LOGIN=mallory", "451 ")
	if data := ehlo(); !reflect.DeepEqual(data, want) {
		t.Fatalf("after a refused XCLIENT, XCLIENTData() = %v, want %v", data, want)
	}
}
