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

// sendMail sends MAIL FROM with params and returns the reply. A MAIL that was
// taken is reset, so a wrong acceptance does not turn the next command into a
// 503.
func sendMail(t *testing.T, c io.Writer, scanner *bufio.Scanner, params string) string {
	t.Helper()
	io.WriteString(c, "MAIL FROM:<a@example.org> "+params+"\r\n")
	res := readReply(t, scanner)
	if strings.HasPrefix(res, "250 ") {
		io.WriteString(c, "RSET\r\n")
		readReply(t, scanner)
	}
	return res
}

// AUTH= on MAIL is the parameter of the AUTH extension (RFC 4954 section 5).
// A connection on which EHLO does not offer AUTH, because the session has no
// mechanism or because the connection is not encrypted and insecure auth is
// off, has not negotiated that extension, so the parameter gets 555 like the
// parameters of the other extensions not offered. RFC 5321 section 4.1.1.2:
// "If service extensions were negotiated, the MAIL command may also carry
// parameters associated with a particular service extension."
func TestServer_MailAuthParamWithoutAUTH555(t *testing.T) {
	refuseAuthParam := func(t *testing.T, c net.Conn, scanner *bufio.Scanner) {
		t.Helper()
		for _, param := range []string{"AUTH=<>", "AUTH=a@example.org", "auth=<>"} {
			if res := sendMail(t, c, scanner, param); !strings.HasPrefix(res, "555 5.5.4 ") {
				t.Errorf("MAIL FROM with %q without AUTH offered: want 555 5.5.4, got %q", param, res)
			}
		}
		// The refused parameters opened no transaction.
		io.WriteString(c, "MAIL FROM:<a@example.org>\r\n")
		if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
			t.Fatalf("MAIL FROM without parameters: want 250, got %q", res)
		}
	}

	t.Run("no mechanism", func(t *testing.T) {
		_, s, c, scanner, caps := testServerEhlo(t, authDisabled)
		defer s.Close()
		defer c.Close()
		if caps["AUTH PLAIN"] {
			t.Fatal("AUTH advertised with auth disabled")
		}
		refuseAuthParam(t, c, scanner)
	})

	t.Run("LMTP, no mechanism", func(t *testing.T) {
		_, s, c, scanner := testServerGreetedLMTP(t, authDisabled, func(s *smtp.Server) {
			s.LMTP = true
		})
		defer s.Close()
		defer c.Close()
		sendLHLO(t, scanner, c)
		refuseAuthParam(t, c, scanner)
	})

	// The parameter follows what EHLO offers on the connection: refused
	// before STARTTLS, taken after it.
	t.Run("insecure auth not allowed", func(t *testing.T) {
		_, s, c, scanner, caps := testServerEhlo(t, withTLS, func(s *smtp.Server) {
			s.AllowInsecureAuth = false
		})
		defer s.Close()
		defer c.Close()
		if caps["AUTH PLAIN"] {
			t.Fatal("AUTH advertised over plaintext without AllowInsecureAuth")
		}
		refuseAuthParam(t, c, scanner)

		tlsConn, tlsScanner := startTLS(t, c, scanner)
		io.WriteString(tlsConn, "EHLO localhost\r\n")
		if caps := readEhloCaps(t, tlsScanner); !caps["AUTH PLAIN"] {
			t.Fatalf("AUTH not advertised after STARTTLS: %v", caps)
		}
		io.WriteString(tlsConn, "MAIL FROM:<a@example.org> AUTH=<>\r\n")
		if res := readReply(t, tlsScanner); !strings.HasPrefix(res, "250 ") {
			t.Fatalf("MAIL FROM with AUTH=<> after STARTTLS: want 250, got %q", res)
		}
	})
}

// SMTPUTF8 (RFC 6531) and REQUIRETLS (RFC 8689) are MAIL parameters without a
// value. RFC 6531 section 3.4: "If this parameter is provided, it MUST not
// accept a value." RFC 8689 section 2: "No value is associated with this
// parameter." A valued one is a syntax error, 501 5.5.4. So is "=" with an
// empty value, for any parameter: RFC 5321 section 4.1.2, "esmtp-value =
// 1*(%d33-60 / %d62-126)".
//
// EHLO offers REQUIRETLS only over TLS, as RFC 8689 section 2 has the option
// specified only when "The session itself MUST employ TLS transmission." Over
// plaintext the parameter belongs to an extension that was not offered: 555.
func TestServer_MailValuelessParams(t *testing.T) {
	_, s, c, scanner, caps := testServerEhlo(t, withTLS, func(s *smtp.Server) {
		s.EnableSMTPUTF8 = true
		s.EnableREQUIRETLS = true
	})
	defer s.Close()
	defer c.Close()

	if caps["REQUIRETLS"] {
		t.Fatal("REQUIRETLS advertised over plaintext")
	}
	if res := sendMail(t, c, scanner, "REQUIRETLS"); !strings.HasPrefix(res, "555 5.5.4 ") {
		t.Errorf("MAIL FROM with REQUIRETLS over plaintext: want 555 5.5.4, got %q", res)
	}

	tlsConn, tlsScanner := startTLS(t, c, scanner)
	io.WriteString(tlsConn, "EHLO localhost\r\n")
	if caps := readEhloCaps(t, tlsScanner); !caps["SMTPUTF8"] || !caps["REQUIRETLS"] {
		t.Fatalf("SMTPUTF8 and REQUIRETLS not advertised over TLS: %v", caps)
	}
	for _, param := range []string{
		"SMTPUTF8=YES",
		"smtputf8=yes",
		"SMTPUTF8=",
		"REQUIRETLS=YES",
		"REQUIRETLS=",
		"SIZE=10 SMTPUTF8=YES",
		"FOO=",
	} {
		if res := sendMail(t, tlsConn, tlsScanner, param); !strings.HasPrefix(res, "501 5.5.4 ") {
			t.Errorf("MAIL FROM with %q: want 501 5.5.4, got %q", param, res)
		}
	}

	// The refused parameters opened no transaction.
	io.WriteString(tlsConn, "MAIL FROM:<a@example.org> SMTPUTF8 REQUIRETLS\r\n")
	if res := readReply(t, tlsScanner); !strings.HasPrefix(res, "250 ") {
		t.Fatalf("MAIL FROM with SMTPUTF8 REQUIRETLS: want 250, got %q", res)
	}
}
