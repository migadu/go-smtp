package smtp_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// The client sends a BDAT chunk right behind the command line, without waiting
// for a reply, so the server must read and drop the chunk of a BDAT it refuses.
// RFC 3030 section 2: "If a failure occurs after a BDAT command is received, the
// receiver-SMTP MUST accept and discard the associated message data before
// sending the appropriate 5XX or 4XX code."
//
// The chunk is command text on purpose: left in the input, it is read as the
// next command, and its reply arrives where the NOOP reply is expected.
const bdatRefusedChunk = "RCPT TO:<other@example.com>\r\n"

const bdatNoopReply = "250 2.0.0 I have successfully done nothing"

var bdatTransaction = []string{
	"MAIL FROM:<sender@example.org>",
	"RCPT TO:<rcpt@example.com>",
}

// startBdatTest connects to a new server and greets it, then sends each of
// cmds, expecting 250.
func startBdatTest(t *testing.T, lmtp bool, maxMessageBytes int64, cmds []string) (*backend, net.Conn, *bufio.Reader) {
	t.Helper()
	be, s, c, _ := testServer(t, func(s *smtp.Server) {
		s.LMTP = lmtp
		s.MaxMessageBytes = maxMessageBytes
	})
	t.Cleanup(func() {
		c.Close()
		s.Close()
	})
	c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)

	if line := bdatReadReply(t, r); !strings.HasPrefix(line, "220 ") {
		t.Fatalf("greeting: %q", line)
	}
	greet := "EHLO"
	if lmtp {
		greet = "LHLO"
	}
	io.WriteString(c, greet+" client.example.org\r\n")
	for {
		line := bdatReadReply(t, r)
		if strings.HasPrefix(line, "250 ") {
			break
		}
		if !strings.HasPrefix(line, "250-") {
			t.Fatalf("%s: %q", greet, line)
		}
	}
	for _, cmd := range cmds {
		io.WriteString(c, cmd+"\r\n")
		if line := bdatReadReply(t, r); !strings.HasPrefix(line, "250 ") {
			t.Fatalf("%s: %q", cmd, line)
		}
	}
	return be, c, r
}

func bdatReadReply(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func TestServer_BDATRefusedChunkDiscarded(t *testing.T) {
	for _, tc := range []struct {
		name            string
		lmtp            bool
		maxMessageBytes int64
		before          []string
		args            string // after the chunk size
		reply           string
	}{
		{"no MAIL", false, 0, nil, " LAST", "502 5.5.1 "},
		{"no RCPT", false, 0, bdatTransaction[:1], " LAST", "502 5.5.1 "},
		{"no RCPT, LMTP", true, 0, bdatTransaction[:1], " LAST", "502 5.5.1 "},
		{"too many arguments", false, 0, bdatTransaction, " LAST LAST", "501 5.5.4 "},
		{"unknown argument", false, 0, bdatTransaction, " FIRST", "501 5.5.4 "},
		{"over MaxMessageBytes", false, 10, bdatTransaction, " LAST", "552 5.3.4 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, r := startBdatTest(t, tc.lmtp, tc.maxMessageBytes, tc.before)

			// One write, as a pipelining client sends it.
			fmt.Fprintf(c, "BDAT %d%s\r\n%sNOOP\r\n", len(bdatRefusedChunk), tc.args, bdatRefusedChunk)
			if line := bdatReadReply(t, r); !strings.HasPrefix(line, tc.reply) {
				t.Fatalf("BDAT: %q, want %q", line, tc.reply)
			}
			if line := bdatReadReply(t, r); line != bdatNoopReply {
				t.Fatalf("next reply after the refused BDAT: %q, want the NOOP reply %q; the chunk was read as commands", line, bdatNoopReply)
			}
		})
	}
}

// The reply to a refused BDAT comes once the whole chunk has been read, not
// while the client is still sending it.
func TestServer_BDATRefusedChunkDiscardedBeforeReply(t *testing.T) {
	for _, tc := range []struct {
		name            string
		maxMessageBytes int64
		before          []string
		reply           string
	}{
		{"no RCPT", 0, bdatTransaction[:1], "502 5.5.1 "},
		{"over MaxMessageBytes", 10, bdatTransaction, "552 5.3.4 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, r := startBdatTest(t, false, tc.maxMessageBytes, tc.before)

			const sent = 10
			fmt.Fprintf(c, "BDAT %d LAST\r\n%s", len(bdatRefusedChunk), bdatRefusedChunk[:sent])
			c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			line, err := r.ReadString('\n')
			if err == nil {
				t.Fatalf("BDAT answered %q with %d of its %d chunk octets still to come", strings.TrimRight(line, "\r\n"), len(bdatRefusedChunk)-sent, len(bdatRefusedChunk))
			}
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				t.Fatalf("waiting for the chunk to be read: %v", err)
			}

			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			io.WriteString(c, bdatRefusedChunk[sent:]+"NOOP\r\n")
			if line := bdatReadReply(t, r); !strings.HasPrefix(line, tc.reply) {
				t.Fatalf("BDAT: %q, want %q", line, tc.reply)
			}
			if line := bdatReadReply(t, r); line != bdatNoopReply {
				t.Fatalf("next reply after the refused BDAT: %q, want the NOOP reply %q", line, bdatNoopReply)
			}
		})
	}
}

// A refused BDAT fails the transaction. RFC 3030 section 2: "If a 5XX or 4XX
// code is received by the sender-SMTP in response to a BDAT chunk, the
// transaction should be considered failed", and the receiver "MUST be prepared
// to accept and discard additional BDAT chunks already in the pipeline after
// the failed BDAT". A BDAT LAST pipelined behind the refused one must not
// complete the transaction with a message that lacks the refused chunk.
func TestServer_BDATRefusalFailsTransaction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lmtp    bool
		refused string // the refused BDAT, with its chunk
		reply   string
	}{
		{"unknown argument", false, "BDAT 5 FIRST\r\nWorld", "501 5.5.4 "},
		{"unknown argument, LMTP", true, "BDAT 5 FIRST\r\nWorld", "501 5.5.4 "},
		{"too many arguments", false, "BDAT 5 LAST LAST\r\nWorld", "501 5.5.4 "},
		{"malformed size", false, "BDAT 5x\r\n", "501 5.5.4 "},
		{"missing size", false, "BDAT\r\n", "501 5.5.4 "},
	} {
		for _, first := range []bool{false, true} {
			name := tc.name
			if first {
				name += ", first chunk"
			}
			t.Run(name, func(t *testing.T) {
				be, c, r := startBdatTest(t, tc.lmtp, 0, bdatTransaction)

				if !first {
					io.WriteString(c, "BDAT 5\r\nHello")
					if line := bdatReadReply(t, r); !strings.HasPrefix(line, "250 ") {
						t.Fatalf("first BDAT: %q", line)
					}
				}

				// One write, as a pipelining client sends it.
				io.WriteString(c, tc.refused+"BDAT 1 LAST\r\n!NOOP\r\n")
				if line := bdatReadReply(t, r); !strings.HasPrefix(line, tc.reply) {
					t.Fatalf("refused BDAT: %q, want %q", line, tc.reply)
				}
				if line := bdatReadReply(t, r); !strings.HasPrefix(line, "50") {
					t.Fatalf("BDAT LAST pipelined behind the refused BDAT: %q, want a refusal (no transaction); the transaction went on without the refused chunk", line)
				}
				if line := bdatReadReply(t, r); line != bdatNoopReply {
					t.Fatalf("next reply after the BDAT LAST: %q, want the NOOP reply %q", line, bdatNoopReply)
				}
				if n := len(be.messages) + len(be.anonmsgs); n != 0 {
					t.Fatalf("%d messages delivered, want none", n)
				}
			})
		}
	}
}
