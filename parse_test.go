package smtp

import (
	"testing"
)

func TestParser(t *testing.T) {
	validReversePaths := []struct {
		raw, path, after string
	}{
		{"<>", "", ""},
		{"<root@nsa.gov>", "root@nsa.gov", ""},
		{"root@nsa.gov", "root@nsa.gov", ""},
		{"<root@nsa.gov> AUTH=asdf@example.org", "root@nsa.gov", " AUTH=asdf@example.org"},
		{"root@nsa.gov AUTH=asdf@example.org", "root@nsa.gov", " AUTH=asdf@example.org"},
	}
	for _, tc := range validReversePaths {
		p := parser{tc.raw}
		path, err := p.parseReversePath()
		if err != nil {
			t.Errorf("parser.parseReversePath(%q) = %v", tc.raw, err)
		} else if path != tc.path {
			t.Errorf("parser.parseReversePath(%q) = %q, want %q", tc.raw, path, tc.path)
		} else if p.s != tc.after {
			t.Errorf("parser.parseReversePath(%q): got after = %q, want %q", tc.raw, p.s, tc.after)
		}
	}

	invalidReversePaths := []string{
		"",
		" ",
		"asdf",
		"<Foo Bar <root@nsa.gov>>",
		" BODY=8BITMIME SIZE=12345",
		"a:b:c@example.org",
		"<root@nsa.gov",
	}
	for _, tc := range invalidReversePaths {
		p := parser{tc}
		if path, err := p.parseReversePath(); err == nil {
			t.Errorf("parser.parseReversePath(%q) = %q, want error", tc, path)
		}
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		raw  string
		want map[string]string
	}{
		{" BODY=8BITMIME SIZE=1024 SMTPUTF8", map[string]string{
			"BODY": "8BITMIME", "SIZE": "1024", "SMTPUTF8": "",
		}},
		// Values may contain '=', e.g. base64 padding in AUTH (RFC 4954)
		// or xtext-free ORCPT values; only the first '=' separates the key.
		{" AUTH=dXNlcg==", map[string]string{"AUTH": "dXNlcg=="}},
		{" ORCPT=rfc822;user=x@example.org", map[string]string{
			"ORCPT": "rfc822;user=x@example.org",
		}},
	}
	for _, tc := range tests {
		got, err := parseArgs(tc.raw)
		if err != nil {
			t.Errorf("parseArgs(%q) = %v", tc.raw, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseArgs(%q) = %v, want %v", tc.raw, got, tc.want)
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("parseArgs(%q)[%q] = %q, want %q", tc.raw, k, got[k], v)
			}
		}
	}
}
