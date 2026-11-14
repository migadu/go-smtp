package smtp

import (
	"bufio"
	"fmt"
	"io"
	"time"
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

// contentReader reads message content, a DATA body or a BDAT chunk, from the
// connection. ReadTimeout bounds each wait for more content rather than the
// whole transfer: RFC 5321 section 4.5.3.2 sets "a timer ... for each buffer
// of the data transfer", so that "the overall timeout is inherently
// proportional to the size of the message". Before every read that has to
// wait for the client, the read deadline is re-armed, capped by the session
// deadline and by Server.DataTimeout counted from the start of the content.
//
// The first error is final: the stream is no longer at a known position, so
// the transfer cannot be resumed. The end of the connection is such an error
// (io.ErrUnexpectedEOF), since content ends at its end-of-data marker or at
// the chunk size, never at EOF.
type contentReader struct {
	c        *Conn
	r        *bufio.Reader
	deadline time.Time // start + Server.DataTimeout; zero if unset
	err      error
}

func newContentReader(c *Conn) *contentReader {
	cr := &contentReader{c: c, r: c.text.R}
	if d := c.server.DataTimeout; d != 0 {
		cr.deadline = time.Now().Add(d)
	}
	return cr
}

// wait arms the read deadline when the next read has to wait for the client.
func (cr *contentReader) wait() error {
	if cr.r.Buffered() > 0 {
		return nil
	}
	return cr.c.armReadDeadline(cr.deadline)
}

func (cr *contentReader) fail(err error) error {
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	cr.err = err
	return err
}

func (cr *contentReader) ReadByte() (byte, error) {
	if cr.err != nil {
		return 0, cr.err
	}
	if err := cr.wait(); err != nil {
		return 0, cr.fail(err)
	}
	b, err := cr.r.ReadByte()
	if err != nil {
		return 0, cr.fail(err)
	}
	return b, nil
}

func (cr *contentReader) Read(p []byte) (int, error) {
	if cr.err != nil {
		return 0, cr.err
	}
	if err := cr.wait(); err != nil {
		return 0, cr.fail(err)
	}
	n, err := cr.r.Read(p)
	if err != nil {
		return n, cr.fail(err)
	}
	return n, nil
}

type dataReader struct {
	r     *contentReader
	state int

	limited bool
	n       int64 // Maximum bytes remaining
}

func newDataReader(c *Conn) *dataReader {
	dr := &dataReader{
		r: newContentReader(c),
	}

	if c.server.MaxMessageBytes > 0 {
		dr.limited = true
		dr.n = int64(c.server.MaxMessageBytes)
	}

	return dr
}

func (r *dataReader) Read(b []byte) (n int, err error) {
	if r.limited {
		if r.n <= 0 {
			return 0, ErrDataTooLarge
		}
		if int64(len(b)) > r.n {
			b = b[0:r.n]
		}
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
