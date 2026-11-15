package smtp_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// refuseEveryRcptSession refuses every recipient, so that a transaction has
// recipients that were all rejected.
type refuseEveryRcptSession struct {
	*session
}

func (s refuseEveryRcptSession) Rcpt(ctx context.Context, to string, opts *smtp.RcptOptions) error {
	return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such user"}
}

// refuseNobodySession refuses the recipient of zeroRcptRefused and accepts
// every other one.
type refuseNobodySession struct {
	*session
}

func (s refuseNobodySession) Rcpt(ctx context.Context, to string, opts *smtp.RcptOptions) error {
	if to == "nobody@example.com" {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such user"}
	}
	return s.session.Rcpt(ctx, to, opts)
}

// startZeroRcptTest connects to a new server whose backend refuses every
// recipient, greets it, and sends each of before, expecting its reply code.
func startZeroRcptTest(t *testing.T, lmtp bool, before [][2]string) (*backend, io.Writer, *bufio.Reader) {
	t.Helper()
	return startRcptPolicyTest(t, lmtp, func(sess *session) smtp.Session {
		return refuseEveryRcptSession{sess}
	}, before)
}

// startRcptPolicyTest is startZeroRcptTest with the session that decides on
// the recipients given by wrap.
func startRcptPolicyTest(t *testing.T, lmtp bool, wrap func(*session) smtp.Session, before [][2]string) (*backend, io.Writer, *bufio.Reader) {
	t.Helper()
	be, s, c, _ := testServer(t, func(s *smtp.Server) {
		s.LMTP = lmtp
		be := s.Backend.(*backend)
		s.Backend = smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
			sess, err := be.NewSession(c)
			if err != nil {
				return nil, err
			}
			return wrap(sess.(*session)), nil
		})
	})
	t.Cleanup(func() {
		c.Close()
		s.Close()
	})
	c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)

	if line := zeroRcptReadReply(t, r); !strings.HasPrefix(line, "220 ") {
		t.Fatalf("greeting: %q", line)
	}
	greet := "EHLO"
	if lmtp {
		greet = "LHLO"
	}
	io.WriteString(c, greet+" client.example.org\r\n")
	for {
		line := zeroRcptReadReply(t, r)
		if strings.HasPrefix(line, "250 ") {
			break
		}
		if !strings.HasPrefix(line, "250-") {
			t.Fatalf("%s: %q", greet, line)
		}
	}
	for _, step := range before {
		io.WriteString(c, step[0]+"\r\n")
		if line := zeroRcptReadReply(t, r); !strings.HasPrefix(line, step[1]+" ") {
			t.Fatalf("%s: %q, want %s", step[0], line, step[1])
		}
	}
	return be, c, r
}

func zeroRcptReadReply(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// isBadSequenceReply accepts the bad-sequence refusal of a BDAT outside a
// transaction with a recipient. The code is 502 here; RFC 3030 section 2 and
// RFC 5321 section 3.3 call for 503, which a separate change makes it.
func isBadSequenceReply(line string) bool {
	return strings.HasPrefix(line, "502 5.5.1 ") || strings.HasPrefix(line, "503 5.5.1 ")
}

const zeroRcptNoopReply = "250 2.0.0 I have successfully done nothing"

var (
	zeroRcptMail    = [2]string{"MAIL FROM:<sender@example.org>", "250"}
	zeroRcptRefused = [2]string{"RCPT TO:<nobody@example.com>", "550"}
)

// RFC 2033 section 4.3: "If there were no previously successful RCPT commands
// in the mail transaction, then the BDAT LAST command returns zero replies."
// A reply there is one more than a pipelining LMTP client counts, and every
// later reply lands on the wrong command.
//
// The chunk is command text on purpose: unless it is read as the chunk and
// dropped, its reply comes before the NOOP's.
func TestServer_LMTPBDATLastWithoutRecipientsNoReply(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before [][2]string
		marker string
		chunk  string
	}{
		{"refused RCPT, empty chunk", [][2]string{zeroRcptMail, zeroRcptRefused}, "LAST", ""},
		{"refused RCPT, chunk", [][2]string{zeroRcptMail, zeroRcptRefused}, "LAST", "RSET\r\n"},
		{"two refused RCPTs", [][2]string{zeroRcptMail, zeroRcptRefused, zeroRcptRefused}, "LAST", "RSET\r\n"},
		{"no RCPT", [][2]string{zeroRcptMail}, "LAST", ""},
		{"lowercase marker", [][2]string{zeroRcptMail, zeroRcptRefused}, "last", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be, c, r := startZeroRcptTest(t, true, tc.before)

			// One write, as a pipelining client sends it.
			fmt.Fprintf(c, "BDAT %d %s\r\n%sNOOP\r\n", len(tc.chunk), tc.marker, tc.chunk)
			if line := zeroRcptReadReply(t, r); line != zeroRcptNoopReply {
				t.Fatalf("first reply after BDAT LAST with no accepted recipient: %q, want the NOOP reply %q", line, zeroRcptNoopReply)
			}

			// BDAT LAST ended the transaction: a new one starts, and a further
			// BDAT is out of sequence (RFC 3030 section 2: "Any BDAT command
			// sent after the BDAT LAST is illegal").
			io.WriteString(c, "BDAT 0 LAST\r\nNOOP\r\n")
			if line := zeroRcptReadReply(t, r); !isBadSequenceReply(line) {
				t.Fatalf("BDAT after BDAT LAST: %q, want a bad-sequence refusal", line)
			}
			if line := zeroRcptReadReply(t, r); line != zeroRcptNoopReply {
				t.Fatalf("NOOP: %q", line)
			}
			io.WriteString(c, "MAIL FROM:<sender@example.org>\r\n")
			if line := zeroRcptReadReply(t, r); !strings.HasPrefix(line, "250 ") {
				t.Fatalf("MAIL after BDAT LAST: %q, want 250 (a new transaction)", line)
			}

			if len(be.messages) != 0 || len(be.anonmsgs) != 0 {
				t.Fatalf("delivered %d messages, want none", len(be.messages)+len(be.anonmsgs))
			}
		})
	}
}

// A chunk before the last one is refused, one reply, but the mail transaction
// stays: the client counts zero replies for the BDAT LAST it pipelined behind.
func TestServer_LMTPBDATLastAfterRefusedChunkNoReply(t *testing.T) {
	_, c, r := startZeroRcptTest(t, true, [][2]string{zeroRcptMail, zeroRcptRefused})

	io.WriteString(c, "BDAT 0\r\nBDAT 0 LAST\r\nNOOP\r\n")
	if line := zeroRcptReadReply(t, r); !isBadSequenceReply(line) {
		t.Fatalf("BDAT 0: %q, want a bad-sequence refusal", line)
	}
	if line := zeroRcptReadReply(t, r); line != zeroRcptNoopReply {
		t.Fatalf("first reply after BDAT LAST: %q, want the NOOP reply %q", line, zeroRcptNoopReply)
	}
}

// A zero-reply chunk the client does not send to its end leaves nothing the
// server could tell from commands: the connection is closed, still without a
// reply. The part of the chunk that was sent is command text on purpose.
func TestServer_LMTPBDATLastWithoutRecipientsChunkCutShort(t *testing.T) {
	_, c, r := startZeroRcptTest(t, true, [][2]string{zeroRcptMail, zeroRcptRefused})

	io.WriteString(c, "BDAT 100 LAST\r\nNOOP\r\n")
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal("CloseWrite:", err)
	}
	if line, err := r.ReadString('\n'); err != io.EOF {
		t.Fatalf("after a BDAT LAST chunk cut short: %q, %v; want the connection closed without a reply", line, err)
	}
}

// The refused chunk leaves the transaction open only for that BDAT LAST. The
// transaction itself has failed (RFC 3030 section 2: "If a 5XX or 4XX code is
// received by the sender-SMTP in response to a BDAT chunk, the transaction
// should be considered failed"): a recipient accepted now would let a BDAT
// LAST complete a message that lacks the refused chunk.
func TestServer_LMTPRcptAfterRefusedChunk(t *testing.T) {
	be, c, r := startRcptPolicyTest(t, true, func(sess *session) smtp.Session {
		return refuseNobodySession{sess}
	}, [][2]string{zeroRcptMail, zeroRcptRefused})

	// One write: the client does not wait for the refusal.
	io.WriteString(c, "BDAT 5\r\nHelloRCPT TO:<rcpt@example.com>\r\nBDAT 1 LAST\r\n!NOOP\r\n")
	if line := zeroRcptReadReply(t, r); !isBadSequenceReply(line) {
		t.Fatalf("BDAT 5: %q, want a bad-sequence refusal", line)
	}
	if line := zeroRcptReadReply(t, r); !strings.HasPrefix(line, "503 5.5.1 ") {
		t.Fatalf("RCPT after the refused chunk: %q, want 503 5.5.1 (the transaction has failed)", line)
	}
	if line := zeroRcptReadReply(t, r); line != zeroRcptNoopReply {
		t.Fatalf("first reply after BDAT LAST: %q, want the NOOP reply %q", line, zeroRcptNoopReply)
	}
	if n := len(be.messages) + len(be.anonmsgs); n != 0 {
		t.Fatalf("delivered %d messages, want none: the message lacks the refused chunk", n)
	}

	// The next transaction takes the recipient.
	for _, cmd := range []string{"MAIL FROM:<sender@example.org>", "RCPT TO:<rcpt@example.com>"} {
		io.WriteString(c, cmd+"\r\n")
		if line := zeroRcptReadReply(t, r); !strings.HasPrefix(line, "250 ") {
			t.Fatalf("%s in the next transaction: %q, want 250", cmd, line)
		}
	}
}

// Outside that one state a BDAT without an accepted recipient keeps its single
// refusal: a chunk that is not the last one (RFC 3030 section 2: "A 250
// response MUST be sent to each successful BDAT data block"; this one is not
// successful), BDAT in SMTP, where the zero-reply rule does not exist, and
// BDAT LAST without MAIL, where there is no mail transaction to end.
func TestServer_BDATWithoutRecipientsOneReply(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lmtp   bool
		before [][2]string
		cmd    string
	}{
		{"LMTP, not the last chunk", true, [][2]string{zeroRcptMail, zeroRcptRefused}, "BDAT 0"},
		{"LMTP, no MAIL", true, nil, "BDAT 0 LAST"},
		{"SMTP, refused RCPT", false, [][2]string{zeroRcptMail, zeroRcptRefused}, "BDAT 0 LAST"},
		{"SMTP, no RCPT", false, [][2]string{zeroRcptMail}, "BDAT 0 LAST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, r := startZeroRcptTest(t, tc.lmtp, tc.before)

			io.WriteString(c, tc.cmd+"\r\nNOOP\r\n")
			if line := zeroRcptReadReply(t, r); !isBadSequenceReply(line) {
				t.Fatalf("%s: %q, want a bad-sequence refusal", tc.cmd, line)
			}
			if line := zeroRcptReadReply(t, r); line != zeroRcptNoopReply {
				t.Fatalf("NOOP: %q", line)
			}
		})
	}
}
