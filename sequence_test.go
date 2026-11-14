package smtp_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// refuseRcptSession refuses one recipient, so that a transaction can have
// recipients that were all rejected.
type refuseRcptSession struct {
	*session
}

func (s refuseRcptSession) Rcpt(ctx context.Context, to string, opts *smtp.RcptOptions) error {
	if to == "refused@example.com" {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such user"}
	}
	return s.session.Rcpt(ctx, to, opts)
}

// A command the server implements, sent outside the transaction state it
// needs, is a bad sequence of commands (503), not an unimplemented command
// (502; RFC 5321 section 4.2.4: "502 SHOULD be used when the command is
// actually recognized by the SMTP server, but not implemented").
//
// RFC 5321 section 3.3: "If a RCPT command appears without a previous MAIL
// command, the server MUST return a 503 "Bad sequence of commands" response."
// For DATA, the same section allows 503 or 554 "If there was no MAIL, or no
// RCPT, command, or all such commands were rejected"; RFC 2033 section 4.2
// makes it a MUST in LMTP: "when there have been no successful RCPT commands
// in the mail transaction, the DATA command MUST fail with a 503 reply code."
// RFC 3030 section 2: "Any BDAT command sent after the BDAT LAST is illegal
// and MUST be replied to with a 503 "Bad sequence of commands" reply code."
//
// The same holds for MAIL and AUTH before the greeting. RFC 5321 section
// 4.1.1.1: "a client MUST issue HELO or EHLO before starting a mail
// transaction"; section 4.3.2 lists 503, not 502, among the error replies to
// MAIL.
func TestServer_BadSequenceReplies(t *testing.T) {
	type step struct{ cmd, code string }
	mail := step{"MAIL FROM:<sender@example.com>", "250"}
	rcpt := step{"RCPT TO:<recipient@example.com>", "250"}
	refused := step{"RCPT TO:<refused@example.com>", "550"}
	// The chunk is "Hello" and the CRLF written after each step.
	bdatLast := step{"BDAT 7 LAST\r\nHello", "250"}

	for _, lmtp := range []bool{false, true} {
		greet := step{"EHLO client.example.org", "250"}
		if lmtp {
			greet = step{"LHLO client.example.org", "250"}
		}
		for _, tc := range []struct {
			name   string
			before []step
			cmd    string
		}{
			{"MAIL before the greeting", nil, "MAIL FROM:<sender@example.com>"},
			{"AUTH before the greeting", nil, "AUTH PLAIN"},
			{"RCPT without MAIL", []step{greet}, "RCPT TO:<recipient@example.com>"},
			{"DATA without MAIL", []step{greet}, "DATA"},
			{"DATA without RCPT", []step{greet, mail}, "DATA"},
			{"DATA after a refused RCPT", []step{greet, mail, refused}, "DATA"},
			{"BDAT without MAIL", []step{greet}, "BDAT 0 LAST"},
			{"BDAT without RCPT", []step{greet, mail}, "BDAT 0 LAST"},
			{"BDAT after a refused RCPT", []step{greet, mail, refused}, "BDAT 0 LAST"},
			{"BDAT after BDAT LAST", []step{greet, mail, rcpt, bdatLast}, "BDAT 0 LAST"},
		} {
			t.Run(fmt.Sprintf("lmtp=%v/%s", lmtp, tc.name), func(t *testing.T) {
				_, s, c, scanner := testServer(t, func(s *smtp.Server) {
					s.LMTP = lmtp
					be := s.Backend.(*backend)
					s.Backend = smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
						sess, err := be.NewSession(c)
						if err != nil {
							return nil, err
						}
						return refuseRcptSession{sess.(*session)}, nil
					})
				})
				defer s.Close()
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))

				readGreetingReply(t, scanner, "220")
				for _, st := range tc.before {
					io.WriteString(c, st.cmd+"\r\n")
					readGreetingReply(t, scanner, st.code)
				}

				io.WriteString(c, tc.cmd+"\r\n")
				if !scanner.Scan() {
					t.Fatalf("%s: no reply: %v", tc.cmd, scanner.Err())
				}
				if got := scanner.Text(); !strings.HasPrefix(got, "503 5.5.1 ") {
					t.Fatalf("%s: %q, want 503 5.5.1 (bad sequence of commands)", tc.cmd, got)
				}
			})
		}
	}
}
