package smtp

import (
	"bufio"
	"fmt"
	"io"
)

type EnhancedCode [3]int

// SMTPError specifies the error code, enhanced error code (if any) and
// message returned by the server.
type SMTPError struct {
	Code         int
	EnhancedCode EnhancedCode
	Message      string
}

// NoEnhancedCode is used to indicate that enhanced error code should not be
// included in response.
//
// Note that RFC 2034 requires an enhanced code to be included in all 2xx, 4xx
// and 5xx responses. This constant is exported for use by extensions, you
// should probably use EnhancedCodeNotSet instead.
var NoEnhancedCode = EnhancedCode{-1, -1, -1}

// EnhancedCodeNotSet is a nil value of EnhancedCode field in SMTPError, used
// to indicate that backend failed to provide enhanced status code. X.0.0 will
// be used (X is derived from error code).
var EnhancedCodeNotSet = EnhancedCode{0, 0, 0}

func (err *SMTPError) Error() string {
	s := fmt.Sprintf("SMTP error %03d", err.Code)
	if err.Message != "" {
		s += ": " + err.Message
	}
	return s
}

func (err *SMTPError) Temporary() bool {
	return err.Code/100 == 4
}

var ErrDataTooLarge = &SMTPError{
	Code:         552,
	EnhancedCode: EnhancedCode{5, 3, 4},
	Message:      "Maximum message size exceeded",
}

type dataReader struct {
	r     *bufio.Reader
	state int

	limited  bool
	n        int64 // Maximum bytes remaining
	tooLarge bool  // A message octet beyond the limit was read
}

func newDataReader(c *Conn) *dataReader {
	dr := &dataReader{
		r: c.text.R,
	}

	if limit := c.server.maxMessageBytes(); limit > 0 {
		dr.limited = true
		dr.n = limit
	}

	return dr
}

func (r *dataReader) Read(b []byte) (n int, err error) {
	if r.limited && r.tooLarge {
		return 0, ErrDataTooLarge
	}

	// Code below is taken from net/textproto with only one modification to
	// not rewrite CRLF -> LF.

	// Run data through a simple state machine to
	// elide leading dots and detect End-of-Data (<CR><LF>.<CR><LF>) line.
	const (
		stateBeginLine = iota // beginning of line; initial state; must be zero
		stateDot              // read . at beginning of line
		stateDotCR            // read .\r at beginning of line
		stateCR               // read \r (possibly at end of line)
		stateData             // reading data in middle of line
		stateEOF              // reached .\r\n end marker line
	)
	for n < len(b) && r.state != stateEOF {
		var c byte
		c, err = r.r.ReadByte()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			break
		}
		switch r.state {
		case stateBeginLine:
			if c == '.' {
				r.state = stateDot
				continue
			}
			if c == '\r' {
				r.state = stateCR
				break
			}
			r.state = stateData
		case stateDot:
			if c == '\r' {
				r.state = stateDotCR
				continue
			}
			r.state = stateData
		case stateDotCR:
			if c == '\n' {
				r.state = stateEOF
				continue
			}
			r.state = stateData
		case stateCR:
			if c == '\n' {
				r.state = stateBeginLine
				break
			}
			r.state = stateData
		case stateData:
			if c == '\r' {
				r.state = stateCR
			}
		}
		if r.limited && int64(n) == r.n {
			// A message octet beyond the limit. Checked here rather than by
			// shortening b, so that the end-of-data line after a message of
			// exactly the limit is still read: it is not part of the message
			// size (RFC 1870 section 5).
			r.tooLarge = true
			err = ErrDataTooLarge
			break
		}
		b[n] = c
		n++
	}
	if err == nil && r.state == stateEOF {
		err = io.EOF
	}

	if r.limited {
		r.n -= int64(n)
	}
	return
}
