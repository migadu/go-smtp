package smtp_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// DATA may not be used in a transaction that BDAT owns. The reply is 503 "Bad
// sequence of commands", not 502 "Command not implemented": the server does
// implement DATA, it is only out of sequence here.
//
// RFC 3030 section 2: "DATA and BDAT commands cannot be used in the same
// transaction. If a DATA statement is issued after a BDAT for the current
// transaction, a 503 "Bad sequence of commands" MUST be issued."
//
// RFC 3030 section 3: "If a DATA command is issued after a MAIL command
// containing the body-value of "BINARYMIME", a 503 "Bad sequence of commands"
// response MUST be sent."
func TestServer_DataInBDATTransaction(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lmtp       bool
		binarymime bool
		mail       string
		chunk      bool // send a non-final BDAT chunk before DATA
	}{
		{"after BDAT", false, false, "MAIL FROM:<sender@example.org>", true},
		{"after BDAT, LMTP", true, false, "MAIL FROM:<sender@example.org>", true},
		{"after MAIL BODY=BINARYMIME", false, true, "MAIL FROM:<sender@example.org> BODY=BINARYMIME", false},
		{"after MAIL BODY=BINARYMIME, LMTP", true, true, "MAIL FROM:<sender@example.org> BODY=BINARYMIME", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configure := func(s *smtp.Server) {
				s.LMTP = tc.lmtp
				s.EnableBINARYMIME = tc.binarymime
			}
			var (
				s       *smtp.Server
				c       net.Conn
				scanner *bufio.Scanner
			)
			if tc.lmtp {
				_, s, c, scanner = testServerGreetedLMTP(t, configure)
				sendLHLO(t, scanner, c)
			} else {
				_, s, c, scanner, _ = testServerEhlo(t, configure)
			}
			defer s.Close()
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))

			dataSeqExpect(t, c, scanner, tc.mail, "250 ")
			dataSeqExpect(t, c, scanner, "RCPT TO:<rcpt@example.com>", "250 ")
			if tc.chunk {
				// The chunk is "Hello\r\n"; dataSeqExpect adds the CRLF.
				dataSeqExpect(t, c, scanner, "BDAT 7\r\nHello", "250 ")
			}

			dataSeqExpect(t, c, scanner, "DATA", "503 5.5.1 ")

			// The state after the 503 is indeterminate; RSET clears it.
			dataSeqExpect(t, c, scanner, "RSET", "250 ")
		})
	}
}

func dataSeqExpect(t *testing.T, c net.Conn, scanner *bufio.Scanner, cmd, reply string) {
	t.Helper()
	io.WriteString(c, cmd+"\r\n")
	if !scanner.Scan() {
		t.Fatalf("%q: no reply: %v", cmd, scanner.Err())
	}
	if !strings.HasPrefix(scanner.Text(), reply) {
		t.Fatalf("%q: got %q, want %q", cmd, scanner.Text(), reply)
	}
}
