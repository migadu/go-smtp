package smtp_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
)

// A MAIL or RCPT parameter the server does not recognize, or recognizes but
// has not enabled, is answered with 555. RFC 5321 section 4.1.1.11: "If the
// server SMTP does not recognize or cannot implement one or more of the
// parameters associated with a particular MAIL FROM or RCPT TO command, it
// will return code 555." Section 4.3.2 lists neither 500 (kept for a line too
// long or an unrecognized command name) nor 504 among the replies to MAIL and
// RCPT. The enhanced code stays 5.5.4, "Invalid command arguments" (RFC 3463).

// mailParamsNotImplemented are MAIL parameters a server with the default
// configuration does not recognize or has not enabled.
var mailParamsNotImplemented = []string{
	"FOO=1",
	"X-UNKNOWN",
	"SMTPUTF8",
	"REQUIRETLS",
	"BODY=BINARYMIME",
	"RET=HDRS",
	"ENVID=QQ314159",
	"BY=120;R",
	"MT-PRIORITY=3",
}

// rcptParamsNotImplemented are RCPT parameters a server with the default
// configuration does not recognize or has not enabled.
var rcptParamsNotImplemented = []string{
	"FOO=1",
	"X-UNKNOWN",
	"XRCPTFORWARD=dXNlcj1qb2hu",
	"NOTIFY=NEVER",
	"ORCPT=rfc822;b@example.org",
	"RRVS=2014-04-03T23:01:00Z",
}

func checkParamsNotImplemented(t *testing.T, c net.Conn, scanner *bufio.Scanner) {
	t.Helper()

	for _, param := range mailParamsNotImplemented {
		io.WriteString(c, "MAIL FROM:<a@example.org> "+param+"\r\n")
		if res := readReply(t, scanner); !strings.HasPrefix(res, "555 5.5.4 ") {
			t.Errorf("MAIL FROM with %q: want 555 5.5.4, got %q", param, res)
		}
	}

	// The refused parameters opened no transaction.
	io.WriteString(c, "MAIL FROM:<a@example.org>\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatalf("MAIL FROM without parameters: want 250, got %q", res)
	}

	for _, param := range rcptParamsNotImplemented {
		io.WriteString(c, "RCPT TO:<b@example.org> "+param+"\r\n")
		if res := readReply(t, scanner); !strings.HasPrefix(res, "555 5.5.4 ") {
			t.Errorf("RCPT TO with %q: want 555 5.5.4, got %q", param, res)
		}
	}

	// The refused parameters did not end the session or the transaction.
	io.WriteString(c, "RCPT TO:<b@example.org>\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatalf("RCPT TO without parameters: want 250, got %q", res)
	}
}

func TestServer_ParamNotImplemented555(t *testing.T) {
	_, s, c, scanner, _ := testServerEhlo(t)
	defer s.Close()
	defer c.Close()

	checkParamsNotImplemented(t, c, scanner)
}

func TestServer_LMTPParamNotImplemented555(t *testing.T) {
	_, s, c, scanner := testServerGreetedLMTP(t, func(s *smtp.Server) {
		s.LMTP = true
	})
	defer s.Close()
	defer c.Close()

	sendLHLO(t, scanner, c)
	checkParamsNotImplemented(t, c, scanner)
}
