package smtp_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// Command names, parameter names, keyword values and SASL mechanism names are
// case-insensitive over US-ASCII only: "ABNF strings are case insensitive and
// the character set for these strings is US-ASCII" (RFC 5234 section 2.3).
// strings.ToUpper maps U+0131 (LATIN SMALL LETTER DOTLESS I) to "I" and U+017F
// (LATIN SMALL LETTER LONG S) to "S", and strings.EqualFold matches U+017F with
// "s": a look-alike must get the reply of an unknown command, parameter, value
// or mechanism, never be taken as the keyword.
func TestServer_KeywordLookalikesAreNotKeywords(t *testing.T) {
	const (
		mail = "MAIL FROM:<root@example.com>"
		rcpt = "RCPT TO:<user@example.com>"
	)
	tests := []struct {
		name  string
		setup []string // each answered 250
		line  string
		want  string // prefix of the reply to line; "" for an unknown parameter's reply
	}{
		{"command name with dotless i", nil, "MAıL FROM:<root@example.com>", "500 5.5.2 "},
		{"command name with long s", nil, "RſET", "500 5.5.2 "},
		{"QUIT with dotless i", nil, "QUıT", "500 5.5.2 "},
		{"MAIL parameter name", nil, mail + " ſIZE=1", ""},
		{"MAIL parameter without value", nil, mail + " ſMTPUTF8", ""},
		{"BODY value", nil, mail + " BODY=8BıTMIME", "501 5.5.4 "},
		{"RET value", nil, mail + " RET=HDRſ", "501 5.5.4 "},
		{"RCPT parameter name", []string{mail}, rcpt + " NOTıFY=NEVER", ""},
		{"NOTIFY value", []string{mail}, rcpt + " NOTIFY=SUCCEſS", "501 5.5.4 "},
		{"BDAT LAST", []string{mail, rcpt}, "BDAT 0 LAſT", "501 5.5.4 "},
		{"AUTH mechanism", nil, "AUTH PLAıN AHVzZXJuYW1lAHBhc3N3b3Jk", "504 5.7.4 "},
		{"XCLIENT attribute name", nil, "XCLIENT LOGıN=alice", "501 5.5.4 "},
		{"XCLIENT PROTO value", nil, "XCLIENT PROTO=EſMTP", "501 5.5.4 "},

		// ASCII letters in any case are still the keywords.
		{"lower-case MAIL and parameters", nil, "mail from:<root@example.com> size=1 smtputf8 body=8bitmime ret=hdrs", "250 "},
		{"lower-case RCPT and NOTIFY", []string{mail}, "rcpt to:<user@example.com> notify=success", "250 "},
		{"lower-case AUTH mechanism", nil, "auth plain AHVzZXJuYW1lAHBhc3N3b3Jk", "235 "},
		{"lower-case XCLIENT attribute", nil, "xclient login=alice proto=esmtp", "220 "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, s, c, scanner, _ := testServerEhlo(t, func(s *smtp.Server) {
				s.EnableSMTPUTF8 = true
				s.EnableDSN = true
				s.EnableXCLIENT = true
				if err := s.AddXCLIENTTrustedNetwork("127.0.0.0/8"); err != nil {
					t.Fatal(err)
				}
			})
			defer s.Close()
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))

			for _, line := range tt.setup {
				io.WriteString(c, line+"\r\n")
				if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "250 ") {
					t.Fatalf("%s: got %q, want 250", line, scanner.Text())
				}
			}

			want := tt.want
			if want == "" {
				// The same line with the look-alike replaced by "X" names
				// an unknown parameter; the look-alike must get that reply,
				// whatever code the server gives an unknown parameter.
				unknown := asciiX.Replace(tt.line)
				io.WriteString(c, unknown+"\r\n")
				if !scanner.Scan() {
					t.Fatalf("%s: connection closed: %v", unknown, scanner.Err())
				}
				want = scanner.Text()
				if !strings.HasPrefix(want, "5") {
					t.Fatalf("%s: got %q, want a 5yz reply", unknown, want)
				}
			}

			io.WriteString(c, tt.line+"\r\n")
			if !scanner.Scan() {
				t.Fatalf("%s: connection closed: %v", tt.line, scanner.Err())
			}
			if got := scanner.Text(); !strings.HasPrefix(got, want) {
				t.Fatalf("%s: got %q, want %q", tt.line, got, want)
			}

			// The connection is still in service.
			io.WriteString(c, "NOOP\r\n")
			if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "250 ") {
				t.Fatalf("NOOP after %s: got %q, want 250", tt.line, scanner.Text())
			}
		})
	}
}

// asciiX replaces the non-ASCII look-alike letters with "X".
var asciiX = strings.NewReplacer("ſ", "X", "ı", "X")
