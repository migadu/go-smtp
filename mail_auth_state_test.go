package smtp_test

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
)

// A session may stop offering another authentication exchange after success.
// That must not revoke MAIL's AUTH parameter from an authenticated client.
type mailAuthAfterSuccessBackend struct{ *backend }
type mailAuthAfterSuccessSession struct{ *session }

func (b mailAuthAfterSuccessBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	s, err := b.backend.NewSession(c)
	if err != nil {
		return nil, err
	}
	return &mailAuthAfterSuccessSession{s.(*session)}, nil
}

func (s *mailAuthAfterSuccessSession) AuthMechanisms() []string {
	if !s.anonymous {
		return nil
	}
	return s.session.AuthMechanisms()
}

func TestMailAUTHParameterAfterSuccessfulAuthentication(t *testing.T) {
	for _, lmtp := range []bool{false, true} {
		name := "SMTP"
		if lmtp {
			name = "LMTP"
		}
		t.Run(name, func(t *testing.T) {
			configure := func(s *smtp.Server) {
				s.LMTP = lmtp
				s.Backend = mailAuthAfterSuccessBackend{s.Backend.(*backend)}
			}
			var srv *smtp.Server
			var c net.Conn
			var scanner *bufio.Scanner
			command := "EHLO"
			if lmtp {
				_, srv, c, scanner = testServerGreetedLMTP(t, configure)
				command = "LHLO"
			} else {
				_, srv, c, scanner = testServerGreeted(t, configure)
			}
			defer srv.Close()
			defer c.Close()
			_, _ = io.WriteString(c, command+" localhost\r\n")
			caps := readEhloCaps(t, scanner)
			if !caps["AUTH PLAIN"] {
				t.Fatal("AUTH was not offered before authentication")
			}
			encoded := base64.StdEncoding.EncodeToString([]byte("\x00username\x00password"))
			_, _ = io.WriteString(c, "AUTH PLAIN "+encoded+"\r\n")
			if got := readReply(t, scanner); !strings.HasPrefix(got, "235 ") {
				t.Fatalf("AUTH: %q", got)
			}
			for _, regreet := range []bool{false, true} {
				if regreet {
					_, _ = io.WriteString(c, command+" localhost\r\n")
					readEhloCaps(t, scanner)
				}
				if got := sendMail(t, c, scanner, "AUTH=<>"); !strings.HasPrefix(got, "250 ") {
					t.Fatalf("authenticated MAIL AUTH parameter: %q, want250", got)
				}
			}
		})
	}
}
