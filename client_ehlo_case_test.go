package smtp

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// startTLSScriptConn replays SMTP replies and records writes and closure.
type startTLSScriptConn struct {
	faker
	written bytes.Buffer
	closed  bool
}

func newStartTLSScriptConn(replies string) *startTLSScriptConn {
	c := &startTLSScriptConn{}
	c.ReadWriter = struct {
		io.Reader
		io.Writer
	}{strings.NewReader(strings.ReplaceAll(replies, "\n", "\r\n")), &c.written}
	return c
}

func (c *startTLSScriptConn) Close() error {
	c.closed = true
	return nil
}

func (c *startTLSScriptConn) commands() string {
	return strings.ReplaceAll(c.written.String(), "\r\n", "\n")
}

func TestClientEHLOKeywordCase(t *testing.T) {
	conn := newStartTLSScriptConn("220 hello\n" +
		"250-mx.example\n250-starttls\n250-8bitmime\n250-Size 1000\n" +
		"250-Auth plain LOGIN\n250 SmtpUtf8\n250 sender ok\n")
	c := NewClient(conn)
	defer c.Close()
	if err := c.Hello("sender.example"); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	for _, ext := range []string{"STARTTLS", "starttls", "8BITMIME", "SMTPUTF8"} {
		if ok, _ := c.Extension(ext); !ok {
			t.Errorf("Extension(%q) = false, want true", ext)
		}
	}
	if size, ok := c.MaxMessageSize(); size != 1000 || !ok {
		t.Errorf("MaxMessageSize = %d, %v; want 1000, true", size, ok)
	}
	if !c.SupportsAuth("PLAIN") || !c.SupportsAuth("login") {
		t.Error("missing AUTH mechanisms from the mixed-case keyword")
	}
	if _, param := c.Extension("AUTH"); param != "plain LOGIN" {
		t.Errorf("AUTH parameter = %q, want %q", param, "plain LOGIN")
	}
	if err := c.Mail("a@sender.example", &MailOptions{UTF8: true, Size: 10}); err != nil {
		t.Fatalf("Mail: %v", err)
	}
	want := "EHLO sender.example\nMAIL FROM:<a@sender.example> BODY=8BITMIME SIZE=10 SMTPUTF8\n"
	if got := conn.commands(); got != want {
		t.Errorf("client wrote %q, want %q", got, want)
	}
}

func TestClientSTARTTLSKeywordCase(t *testing.T) {
	conn := newStartTLSScriptConn("220 hello\n250-mx.example\n250 StartTLS\n220 go ahead\n")
	c, err := NewClientStartTLSWithName(conn, nil, "sender.example")
	if err != nil {
		t.Fatalf("NewClientStartTLSWithName: %v", err)
	}
	defer c.Close()
	if got, want := conn.commands(), "EHLO sender.example\nSTARTTLS\n"; got != want {
		t.Errorf("client wrote %q, want %q", got, want)
	}
}
