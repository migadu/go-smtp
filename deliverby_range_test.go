package smtp

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// RFC 2852 section 4 limits BY to nine digits, permits negative times in
// Notify mode, and requires a strictly positive whole-second Return time.
func TestParseDeliverByRange(t *testing.T) {
	for _, tc := range []struct {
		arg     string
		seconds int64
		mode    DeliverByMode
		valid   bool
	}{
		{"999999999;R", 999999999, DeliverByReturn, true},
		{"999999999;N", 999999999, DeliverByNotify, true},
		{"-999999999;N", -999999999, DeliverByNotify, true},
		{"0;N", 0, DeliverByNotify, true},
		{"1;R", 1, DeliverByReturn, true},
		{"1000000000;R", 0, "", false},
		{"1000000000;N", 0, "", false},
		{"-1000000000;N", 0, "", false},
		{"-999999999;R", 0, "", false},
		{"0;R", 0, "", false},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			got := parseDeliverByArgument(tc.arg)
			if !tc.valid {
				if got != nil {
					t.Fatalf("accepted BY=%s: %#v", tc.arg, got)
				}
				return
			}
			if got == nil || got.Time != time.Duration(tc.seconds)*time.Second || got.Mode != tc.mode {
				t.Fatalf("parsed BY=%s as %#v, want %d seconds in mode %s", tc.arg, got, tc.seconds, tc.mode)
			}
		})
	}
}

// Invalid requested times must be rejected locally: they must never open a
// transaction by sending MAIL with a BY value outside the wire format's range.
func TestClientDeliverByRange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		time  time.Duration
		mode  DeliverByMode
		valid bool
	}{
		{"return maximum", 999999999 * time.Second, DeliverByReturn, true},
		{"notify maximum", 999999999 * time.Second, DeliverByNotify, true},
		{"notify minimum", -999999999 * time.Second, DeliverByNotify, true},
		{"notify zero", 0, DeliverByNotify, true},
		{"return minimum", time.Second, DeliverByReturn, true},
		{"return above maximum", 1000000000 * time.Second, DeliverByReturn, false},
		{"notify above maximum", 1000000000 * time.Second, DeliverByNotify, false},
		{"notify below minimum", -1000000000 * time.Second, DeliverByNotify, false},
		{"return zero", 0, DeliverByReturn, false},
		{"return subsecond", 500 * time.Millisecond, DeliverByReturn, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wrote bytes.Buffer
			var fake faker
			fake.ReadWriter = struct {
				io.Reader
				io.Writer
			}{strings.NewReader("250 accepted\r\n"), &wrote}
			c := NewClient(fake)
			defer c.Close()
			c.didHello = true
			c.ext = map[string]string{"DELIVERBY": ""}
			err := c.Mail("sender@example.com", &MailOptions{
				DeliverBy: &DeliverByOptions{Time: tc.time, Mode: tc.mode},
			})
			if !tc.valid {
				if err == nil || wrote.Len() != 0 {
					t.Fatalf("invalid time sent %q, error %v", wrote.String(), err)
				}
				return
			}
			want := fmt.Sprintf("MAIL FROM:<sender@example.com> BY=%d;%s\r\n", tc.time/time.Second, tc.mode)
			if err != nil || wrote.String() != want {
				t.Fatalf("Mail sent %q, error %v; want %q", wrote.String(), err, want)
			}
		})
	}
}
