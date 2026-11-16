package smtp_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
)

// A MAIL or RCPT parameter given twice is a syntax error, 501 5.5.4. RFC 6152
// section 3: "Only one BODY parameter may be used in a single MAIL command."
// RFC 1870 section 6: "Only one SIZE parameter may be specified in a single
// MAIL command." RFC 3461 section 4.5: "The RET and ENVID parameters MUST NOT
// appear more than once each in any single MAIL command. If more than one of
// either of these parameters appears in a MAIL command, the ESMTP server
// SHOULD respond with "501 syntax error in parameters or arguments"", and the
// same for NOTIFY and ORCPT on RCPT. Any other keyword given twice is as
// ambiguous, since the server cannot tell which value the client meant, so it
// is refused too, whatever the keyword. Keywords are compared case-insensitively
// (RFC 5321 section 2.4).
func checkRepeatedParams501(t *testing.T, c net.Conn, scanner *bufio.Scanner) {
	t.Helper()

	for _, params := range []string{
		"BODY=7BIT BODY=8BITMIME",
		"BODY=8BITMIME body=8BITMIME",
		"SIZE=10 SIZE=20",
		"SIZE=10 size=10",
		"SMTPUTF8 SMTPUTF8",
		"SMTPUTF8 BODY=8BITMIME smtputf8",
		"RET=HDRS RET=FULL",
		"ENVID=QQ314159 ENVID=QQ314159",
		"FOO=1 FOO=2",
		"X-UNKNOWN x-unknown",
	} {
		if res := sendMail(t, c, scanner, params); !strings.HasPrefix(res, "501 5.5.4 ") {
			t.Errorf("MAIL FROM with %q: want 501 5.5.4, got %q", params, res)
		}
	}

	// Each parameter once is taken.
	io.WriteString(c, "MAIL FROM:<a@example.org> BODY=8BITMIME SIZE=10 SMTPUTF8 RET=HDRS ENVID=QQ314159\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatalf("MAIL FROM with each parameter once: want 250, got %q", res)
	}

	for _, params := range []string{
		"NOTIFY=NEVER NOTIFY=SUCCESS",
		"notify=never NOTIFY=NEVER",
		"ORCPT=rfc822;b@example.org ORCPT=rfc822;c@example.org",
		"XRCPTFORWARD=dXNlcj1qb2hu XRCPTFORWARD=dXNlcj1qYW5l",
		"xrcptforward=dXNlcj1qb2hu XRCPTFORWARD=dXNlcj1qb2hu",
		"FOO=1 foo=1",
	} {
		io.WriteString(c, "RCPT TO:<b@example.org> "+params+"\r\n")
		if res := readReply(t, scanner); !strings.HasPrefix(res, "501 5.5.4 ") {
			t.Errorf("RCPT TO with %q: want 501 5.5.4, got %q", params, res)
		}
	}

	// The refused recipients did not end the transaction, and each parameter
	// once is taken.
	io.WriteString(c, "RCPT TO:<b@example.org> NOTIFY=NEVER ORCPT=rfc822;b@example.org XRCPTFORWARD=dXNlcj1qb2hu\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatalf("RCPT TO with each parameter once: want 250, got %q", res)
	}
}

func enableRepeatedParamExtensions(s *smtp.Server) {
	s.EnableSMTPUTF8 = true
	s.EnableDSN = true
	s.EnableRCPTExtensions = true
}

func TestServer_RepeatedParams501(t *testing.T) {
	_, s, c, scanner, _ := testServerEhlo(t, enableRepeatedParamExtensions)
	defer s.Close()
	defer c.Close()

	checkRepeatedParams501(t, c, scanner)
}

func TestServer_LMTPRepeatedParams501(t *testing.T) {
	_, s, c, scanner := testServerGreetedLMTP(t, enableRepeatedParamExtensions, func(s *smtp.Server) {
		s.LMTP = true
	})
	defer s.Close()
	defer c.Close()

	sendLHLO(t, scanner, c)
	checkRepeatedParams501(t, c, scanner)
}
