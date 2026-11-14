package smtp_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
)

// RFC 1870 section 4: the SIZE parameter "indicates the size of the largest
// message that the server is willing to accept", and section 5 counts "the
// number of octets, including CR-LF pairs, but not the SMTP DATA command's
// terminating dot or doubled quoting dots". A message of exactly
// MaxMessageBytes is therefore accepted; one octet more is refused, and the
// rest of the refused body is not read as commands.
func TestServer_DATA_exactlyMaxMessageBytes(t *testing.T) {
	// A dot-stuffed line: the quoting dot is not part of the message size.
	const content = ".23456789012345678\r\n"
	const wire = "." + content

	for _, lmtp := range []bool{false, true} {
		name := "SMTP"
		if lmtp {
			name = "LMTP"
		}
		t.Run(name, func(t *testing.T) {
			configure := func(s *smtp.Server) {
				s.LMTP = lmtp
				s.MaxMessageBytes = int64(len(content))
			}
			var (
				be      *backend
				s       *smtp.Server
				c       net.Conn
				scanner *bufio.Scanner
			)
			if lmtp {
				be, s, c, scanner = testServerGreetedLMTP(t, configure)
				sendLHLO(t, scanner, c)
			} else {
				be, s, c, scanner, _ = testServerEhlo(t, configure)
			}
			defer s.Close()
			defer c.Close()

			send := func(body string) string {
				io.WriteString(c, "MAIL FROM:<root@example.org>\r\n")
				scanner.Scan()
				io.WriteString(c, "RCPT TO:<root@example.com>\r\n")
				scanner.Scan()
				io.WriteString(c, "DATA\r\n")
				scanner.Scan()
				if !strings.HasPrefix(scanner.Text(), "354 ") {
					t.Fatal("Invalid DATA response:", scanner.Text())
				}
				io.WriteString(c, body+".\r\n")
				scanner.Scan()
				return scanner.Text()
			}

			if resp := send(wire); !strings.HasPrefix(resp, "250 ") {
				t.Fatalf("message of exactly MaxMessageBytes (%d): %q, want 250", len(content), resp)
			}
			if resp := send("x" + wire + "MAIL FROM:<smuggled@example.org>\r\n"); !strings.HasPrefix(resp, "552 5.3.4 ") {
				t.Fatalf("message over MaxMessageBytes: %q, want 552 5.3.4", resp)
			}

			io.WriteString(c, "NOOP\r\n")
			scanner.Scan()
			if !strings.HasPrefix(scanner.Text(), "250 ") {
				t.Fatal("Invalid NOOP response:", scanner.Text())
			}

			msgs := be.anonmsgs
			if len(be.messages) != 0 || len(msgs) != 1 {
				t.Fatal("Invalid number of sent messages:", be.messages, be.anonmsgs)
			}
			if string(msgs[0].Data) != content {
				t.Fatalf("message data = %q, want %q", msgs[0].Data, content)
			}
		})
	}
}
