package smtp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// toUpperASCII returns s with its ASCII letters upper-cased and every other
// byte unchanged. Command names, parameter names and keyword values are
// case-insensitive over US-ASCII only: "ABNF strings are case insensitive and
// the character set for these strings is US-ASCII" (RFC 5234 section 2.3).
// strings.ToUpper would also map U+0131 (LATIN SMALL LETTER DOTLESS I) to "I"
// and U+017F (LATIN SMALL LETTER LONG S) to "S", turning a look-alike such as
// "MAıL" into a command.
func toUpperASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if 'a' <= s[i] && s[i] <= 'z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'a' <= b[j] && b[j] <= 'z' {
					b[j] -= 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// equalFoldASCII reports whether s and t are equal when ASCII letters are
// compared case-insensitively and every other byte exactly. Unlike
// strings.EqualFold it does not take U+017F (LATIN SMALL LETTER LONG S) for
// "s" or U+212A (KELVIN SIGN) for "k"; see toUpperASCII.
func equalFoldASCII(s, t string) bool {
	if len(s) != len(t) {
		return false
	}
	for i := 0; i < len(s); i++ {
		a, b := s[i], t[i]
		if 'a' <= a && a <= 'z' {
			a -= 'a' - 'A'
		}
		if 'a' <= b && b <= 'z' {
			b -= 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// cutPrefixFold is a version of strings.CutPrefix which is case-insensitive
// over ASCII letters (see equalFoldASCII).
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !equalFoldASCII(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// parseSize parses the size declared by the SIZE parameter of MAIL
// (RFC 1870 section 3: "size-value ::= 1*20DIGIT") or by BDAT (RFC 3030
// section 2: "chunk-size ::= 1*DIGIT"). Neither grammar is bounded by the
// integer types, so a size too large for an int64 is valid syntax: tooLarge
// reports it, and it exceeds any limit the server can have.
func parseSize(s string) (size int64, tooLarge bool, err error) {
	// ParseUint, unlike ParseInt, refuses a sign; a bit size of 63 keeps
	// the result within an int64.
	n, err := strconv.ParseUint(s, 10, 63)
	if err != nil {
		// ParseUint reports the range error as soon as the digits read so
		// far overflow, before it looks at the rest of s: only a string of
		// digits alone is a size too large rather than a syntax error.
		if errors.Is(err, strconv.ErrRange) && strings.Trim(s, "0123456789") == "" {
			return 0, true, nil
		}
		return 0, false, err
	}
	return int64(n), false, nil
}

func parseCmd(line string) (cmd string, arg string, err error) {
	line = strings.TrimRight(line, "\r\n")

	if len(line) == 0 {
		return "", "", nil
	}

	// Find the first space to separate command from arguments
	spaceIndex := strings.IndexByte(line, ' ')

	if spaceIndex == -1 {
		// No space found, entire line is the command
		if len(line) < 4 {
			return "", "", fmt.Errorf("command too short: %q", line)
		}
		return toUpperASCII(line), "", nil
	}

	// Space found, split into command and arguments
	if spaceIndex < 4 {
		return "", "", fmt.Errorf("command too short: %q", line)
	}

	command := toUpperASCII(line[:spaceIndex])
	arguments := strings.TrimSpace(line[spaceIndex+1:])

	return command, arguments, nil
}

// Takes the arguments proceeding a command and files them
// into a map[string]string after uppercasing each key.  Sample arg
// string:
//
//	" BODY=8BITMIME SIZE=1024 SMTPUTF8"
//
// The leading space is mandatory.
func parseArgs(s string) (map[string]string, error) {
	argMap := map[string]string{}
	for _, arg := range strings.Fields(s) {
		// Split on the first '=' only: parameter values may themselves
		// contain '=' (e.g. AUTH=dXNlcg== with base64 padding, RFC 4954,
		// or an ORCPT address).
		key, value, found := strings.Cut(arg, "=")
		if found {
			argMap[toUpperASCII(key)] = value
		} else {
			argMap[toUpperASCII(key)] = ""
		}
	}
	return argMap, nil
}

func parseHelloArgument(arg string) (string, error) {
	domain := arg
	if idx := strings.IndexRune(arg, ' '); idx >= 0 {
		domain = arg[:idx]
	}
	if domain == "" {
		return "", fmt.Errorf("invalid domain")
	}
	return domain, nil
}

// Parses the BY argument defined in RFC2852 section 4.
// Returns pointer to options or nil if invalid.
func parseDeliverByArgument(arg string) *DeliverByOptions {
	secondsStr, modeStr, ok := strings.Cut(arg, ";")
	if !ok {
		return nil
	}
	modeStr, traceValue := strings.CutSuffix(modeStr, "T")
	if modeStr != string(DeliverByNotify) && modeStr != string(DeliverByReturn) {
		return nil
	}
	modeValue := DeliverByMode(modeStr)
	secondsValue, err := strconv.Atoi(secondsStr)
	// Specified in rfc2852
	// by-time      = ["-" / "+"]1*9digit ; a negative or zero value is not
	//				      ; allowed with a by-mode of "R"
	if err != nil ||
		(modeValue == DeliverByReturn && (secondsValue < 1 || secondsValue > 999999999)) ||
		(modeValue == DeliverByNotify && (secondsValue < -999999999 || secondsValue > 999999999)) {
		return nil
	}
	return &DeliverByOptions{
		Time:  time.Duration(secondsValue) * time.Second,
		Mode:  modeValue,
		Trace: traceValue,
	}
}

// parser parses command arguments defined in RFC 5321 section 4.1.2.
type parser struct {
	s string
}

func (p *parser) peekByte() (byte, bool) {
	if len(p.s) == 0 {
		return 0, false
	}
	return p.s[0], true
}

func (p *parser) readByte() (byte, bool) {
	ch, ok := p.peekByte()
	if ok {
		p.s = p.s[1:]
	}
	return ch, ok
}

func (p *parser) acceptByte(ch byte) bool {
	got, ok := p.peekByte()
	if !ok || got != ch {
		return false
	}
	p.readByte()
	return true
}

func (p *parser) expectByte(ch byte) error {
	if !p.acceptByte(ch) {
		if len(p.s) == 0 {
			return fmt.Errorf("expected '%v', got EOF", string(ch))
		} else {
			return fmt.Errorf("expected '%v', got '%v'", string(ch), string(p.s[0]))
		}
	}
	return nil
}

func (p *parser) parseReversePath() (string, error) {
	if strings.HasPrefix(p.s, "<>") {
		p.s = strings.TrimPrefix(p.s, "<>")
		return "", nil
	}
	return p.parsePath()
}

// parseForwardPath parses the argument of RCPT TO. Unlike a reverse-path, it
// may be "<Postmaster>" without a domain (RFC 5321 section 4.1.1.3), which is
// returned as given.
func (p *parser) parseForwardPath() (string, error) {
	if rest, ok := cutPrefixFold(p.s, "<postmaster>"); ok {
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
			mbox := p.s[1 : 1+len("postmaster")]
			p.s = rest
			return mbox, nil
		}
	}
	return p.parsePath()
}

func (p *parser) parsePath() (string, error) {
	hasBracket := p.acceptByte('<')
	if p.acceptByte('@') {
		i := strings.IndexByte(p.s, ':')
		if i < 0 {
			return "", fmt.Errorf("malformed a-d-l")
		}
		for j := 0; j < i; j++ {
			if p.s[j] <= 0x20 || p.s[j] == 0x7f {
				return "", fmt.Errorf("malformed a-d-l")
			}
		}
		p.s = p.s[i+1:]
	}
	mbox, err := p.parseMailbox()
	if err != nil {
		return "", fmt.Errorf("in mailbox: %v", err)
	}
	if hasBracket {
		if err := p.expectByte('>'); err != nil {
			return "", err
		}
	} else if strings.TrimSpace(p.s) != "" {
		// Preserve legacy bare mailbox compatibility only without parameters.
		// ESMTP parameters require the path's explicit closing delimiter.
		return "", fmt.Errorf("path with parameters must be enclosed in angle brackets")
	}
	return mbox, nil
}

func (p *parser) parseMailbox() (string, error) {
	localPart, err := p.parseLocalPart()
	if err != nil {
		return "", fmt.Errorf("in local-part: %v", err)
	} else if localPart == "" {
		return "", fmt.Errorf("local-part is empty")
	}

	if err := p.expectByte('@'); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(localPart)
	sb.WriteByte('@')

	for {
		ch, ok := p.peekByte()
		if !ok {
			break
		}
		if ch == ' ' || ch == '\t' || ch == '>' {
			break
		}
		if ch < 0x20 || ch == 0x7f {
			return "", fmt.Errorf("malformed domain")
		}
		p.readByte()
		sb.WriteByte(ch)
	}

	if strings.HasSuffix(sb.String(), "@") {
		return "", fmt.Errorf("domain is empty")
	}

	return sb.String(), nil
}

func (p *parser) parseLocalPart() (string, error) {
	var sb strings.Builder

	if p.acceptByte('"') { // quoted-string
		for {
			ch, ok := p.readByte()
			switch ch {
			case '\\':
				ch, ok = p.readByte()
			case '"':
				return sb.String(), nil
			}
			if !ok || ch < 0x20 || ch == 0x7f {
				return "", fmt.Errorf("malformed quoted-string")
			}
			sb.WriteByte(ch)
		}
	} else { // dot-string
		for {
			ch, ok := p.peekByte()
			if !ok {
				break
			}
			switch ch {
			case '@', '>':
				goto done
			case '(', ')', '<', '[', ']', ':', ';', '\\', ',', '"', ' ', '\t':
				return "", fmt.Errorf("malformed dot-string")
			}
			if ch < 0x20 || ch == 0x7f {
				return "", fmt.Errorf("malformed dot-string")
			}
			p.readByte()
			sb.WriteByte(ch)
		}
	done:
		// Leading, trailing and consecutive dots are not a valid RFC 5321
		// Dot-string, but they are accepted on purpose, as Postfix does:
		// older Japanese carrier addresses (docomo, au/ezweb, SoftBank)
		// still use them in large numbers, and refusing them at MAIL FROM
		// bounces their mail. Every other refusal
		// above stands.
		return sb.String(), nil
	}
}
