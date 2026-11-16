package smtp_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
)

// paramOrderRepeats is how many times each command is sent: the parameters
// were walked in map order, which Go randomises, so a single attempt could
// pass by chance.
const paramOrderRepeats = 50

// When a MAIL or RCPT command carries more than one parameter the server
// refuses, the first one refused, in the order the client sent them, decides
// the reply, so one command gets the same reply every time. RFC 5321 section
// 4.2.2 has "501 Syntax error in parameters or arguments" and "555 MAIL
// FROM/RCPT TO parameters not recognized or not implemented", and section
// 4.1.1.11: "If the server SMTP does not recognize or cannot implement one or
// more of the parameters associated with a particular MAIL FROM or RCPT TO
// command, it will return code 555." Neither says which code wins when one
// parameter is malformed and another is not recognized.
func checkParamOrder(t *testing.T, c net.Conn, scanner *bufio.Scanner) {
	t.Helper()

	for _, tt := range []struct{ params, want string }{
		{"SIZE=abc FOO=1", "501 5.5.4 "},
		{"FOO=1 SIZE=abc", "555 5.5.4 "},
		{"SIZE=2000 FOO=1", "552 5.3.4 "},
		{"FOO=1 SIZE=2000", "555 5.5.4 "},
		{"SMTPUTF8=YES BODY=BINARYMIME", "501 5.5.4 "},
		{"BODY=BINARYMIME SMTPUTF8=YES", "555 5.5.4 "},
		{"BODY=9BIT RET=HDRS", "501 5.5.4 "},
		{"RET=HDRS BODY=9BIT", "555 5.5.4 "},
	} {
		for i := 0; i < paramOrderRepeats; i++ {
			if res := sendMail(t, c, scanner, tt.params); !strings.HasPrefix(res, tt.want) {
				t.Errorf("MAIL FROM with %q (attempt %d): want %q, got %q", tt.params, i+1, tt.want, res)
				break
			}
		}
	}

	io.WriteString(c, "MAIL FROM:<a@example.org>\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatalf("MAIL FROM without parameters: want 250, got %q", res)
	}
	for _, tt := range []struct{ params, want string }{
		{"XRCPTFORWARD=!!! NOTIFY=NEVER", "501 5.5.4 "},
		{"NOTIFY=NEVER XRCPTFORWARD=!!!", "555 5.5.4 "},
	} {
		for i := 0; i < paramOrderRepeats; i++ {
			io.WriteString(c, "RCPT TO:<b@example.org> "+tt.params+"\r\n")
			if res := readReply(t, scanner); !strings.HasPrefix(res, tt.want) {
				t.Errorf("RCPT TO with %q (attempt %d): want %q, got %q", tt.params, i+1, tt.want, res)
				break
			}
		}
	}
}

func enableParamOrderExtensions(s *smtp.Server) {
	s.EnableSMTPUTF8 = true
	s.EnableRCPTExtensions = true
	s.MaxMessageBytes = 1000
}

func TestServer_ParamOrder(t *testing.T) {
	_, s, c, scanner, _ := testServerEhlo(t, enableParamOrderExtensions)
	defer s.Close()
	defer c.Close()

	checkParamOrder(t, c, scanner)
}

func TestServer_LMTPParamOrder(t *testing.T) {
	_, s, c, scanner := testServerGreetedLMTP(t, enableParamOrderExtensions, func(s *smtp.Server) {
		s.LMTP = true
	})
	defer s.Close()
	defer c.Close()

	sendLHLO(t, scanner, c)
	checkParamOrder(t, c, scanner)
}
