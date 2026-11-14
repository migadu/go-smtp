package smtp_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// RFC 1870 section 3 gives the SIZE parameter of MAIL as
// "size-value ::= 1*20DIGIT", and RFC 3030 section 2 the BDAT chunk size as
// "chunk-size ::= 1*DIGIT": neither is bounded by 32 bits. A size beyond the
// limit is answered as RFC 1870 section 6.1 (2) says ("552 message size
// exceeds fixed maximium message size"), not refused as a syntax error.

func TestServer_MAILSizeBeyond32Bits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
		size  string
		want  string
	}{
		{"over the limit", 50 << 20, "5000000000", "552 5.3.4 "},
		{"under a limit beyond 32 bits", 8 << 30, "5000000000", "250 "},
		{"no limit", 0, "5000000000", "250 "},
		{"largest int64", 0, "9223372036854775807", "250 "},
		{"beyond int64", 50 << 20, "9223372036854775808", "552 5.3.4 "},
		{"beyond int64 without a limit", 0, "9223372036854775808", "552 5.3.4 "},
		{"beyond uint64 in 20 digits", 50 << 20, "99999999999999999999", "552 5.3.4 "},
		{"not a number", 50 << 20, "rabbit", "501 5.5.4 "},
		{"negative", 0, "-1", "501 5.5.4 "},
		{"signed", 0, "+1", "501 5.5.4 "},
		{"digits beyond uint64, then a letter", 50 << 20, "99999999999999999999x", "501 5.5.4 "},
		{"digits beyond int64, then a sign", 0, "9223372036854775808-", "501 5.5.4 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be, s, c, scanner, _ := testServerEhlo(t, func(s *smtp.Server) {
				s.MaxMessageBytes = tc.limit
			})
			defer s.Close()
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))

			io.WriteString(c, "MAIL FROM:<root@nsa.gov> SIZE="+tc.size+"\r\n")
			scanner.Scan()
			if !strings.HasPrefix(scanner.Text(), tc.want) {
				t.Fatalf("MAIL SIZE=%s with MaxMessageBytes %d: %q, want %q", tc.size, tc.limit, scanner.Text(), tc.want)
			}
			if tc.want != "250 " {
				return
			}

			// The declared size reaches the backend intact.
			io.WriteString(c, "RCPT TO:<root@gchq.gov.uk>\r\n")
			scanner.Scan()
			io.WriteString(c, "DATA\r\n")
			scanner.Scan()
			io.WriteString(c, "Hey <3\r\n.\r\n")
			scanner.Scan()
			if !strings.HasPrefix(scanner.Text(), "250 ") {
				t.Fatal("Invalid DATA response:", scanner.Text())
			}
			if len(be.anonmsgs) != 1 {
				t.Fatal("Invalid number of sent messages:", be.messages, be.anonmsgs)
			}
			if got := be.anonmsgs[0].Opts.Size; strconv.FormatInt(got, 10) != tc.size {
				t.Fatalf("MailOptions.Size = %d, want %s", got, tc.size)
			}
		})
	}
}

func TestServer_BDATChunkBeyond32Bits(t *testing.T) {
	_, s, c, scanner := testBDATTransaction(t, 10)
	defer s.Close()
	defer c.Close()

	// The 552 is written before the server starts discarding the chunk, so
	// it arrives without the chunk being sent.
	io.WriteString(c, "BDAT 5000000000\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "552 5.3.4 ") {
		t.Fatal("Invalid BDAT response:", scanner.Text())
	}
}

// The limit check must not overflow when a chunk size close to the largest
// int64 is added to the octets already received.
func TestServer_BDATChunkOverflowingLimitCheck(t *testing.T) {
	_, s, c, scanner := testBDATTransaction(t, 50)
	defer s.Close()
	defer c.Close()

	io.WriteString(c, "BDAT 8\r\nHey <3\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("Invalid BDAT response:", scanner.Text())
	}

	io.WriteString(c, "BDAT 9223372036854775800\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "552 5.3.4 ") {
		t.Fatal("Invalid BDAT response:", scanner.Text())
	}
}

// A chunk too large for an int64 exceeds any limit, and the server cannot
// read past it to the next command, so it refuses it and closes the
// connection rather than parse the chunk as commands.
func TestServer_BDATChunkBeyondInt64(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
		size  string
	}{
		{"beyond int64", 50, "9223372036854775808"},
		{"beyond int64 without a limit", 0, "9223372036854775808"},
		{"beyond uint64", 0, "123456789012345678901234567890"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, c, scanner := testBDATTransaction(t, tc.limit)
			defer s.Close()
			defer c.Close()

			io.WriteString(c, "BDAT "+tc.size+"\r\n")
			scanner.Scan()
			if !strings.HasPrefix(scanner.Text(), "552 5.3.4 ") {
				t.Fatal("Invalid BDAT response:", scanner.Text())
			}
			if scanner.Scan() {
				t.Fatal("Expected the connection to be closed, got:", scanner.Text())
			}
			var ne net.Error
			if err := scanner.Err(); errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("Connection left open after an unreadable chunk")
			}
		})
	}
}

// A size argument that is not all digits is a syntax error, however many
// digits precede the first other character: it is not a valid oversized
// chunk, and the connection stays usable.
func TestServer_BDATMalformedOverlongSize(t *testing.T) {
	_, s, c, scanner := testBDATTransaction(t, 0)
	defer s.Close()
	defer c.Close()

	io.WriteString(c, "BDAT 99999999999999999999x\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "501 5.5.4 ") {
		t.Fatal("Invalid BDAT response:", scanner.Text())
	}
	io.WriteString(c, "NOOP\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("Invalid NOOP response:", scanner.Text())
	}
}

// Once the server has refused an unreadable chunk and closed the connection,
// commands the client pipelined behind the BDAT, already in the server's
// read buffer, are not handled: the session they would run on is gone.
func TestServer_BDATChunkBeyondInt64IgnoresPipelinedCommands(t *testing.T) {
	errorLog := &lockedBuffer{}
	_, s, c, scanner := testBDATTransaction(t, 0, func(s *smtp.Server) {
		s.ErrorLog = log.New(errorLog, "", 0)
	})
	defer s.Close()
	defer c.Close()

	io.WriteString(c, "BDAT 9223372036854775808\r\nRCPT TO:<root@gchq.gov.uk>\r\nNOOP\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "552 5.3.4 ") {
		t.Fatal("Invalid BDAT response:", scanner.Text())
	}
	if scanner.Scan() {
		t.Fatal("Expected the connection to be closed, got:", scanner.Text())
	}

	// Shutdown returns once the connection goroutine is done with what it
	// had buffered.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal("Shutdown:", err)
	}
	if msg := errorLog.String(); msg != "" {
		t.Fatal("Pipelined commands handled after the connection was closed:", msg)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testBDATTransaction starts a transaction ready for BDAT on a server with
// the given MaxMessageBytes.
func testBDATTransaction(t *testing.T, limit int64, fn ...serverConfigureFunc) (*backend, *smtp.Server, net.Conn, *bufio.Scanner) {
	be, s, c, scanner, _ := testServerEhlo(t, append([]serverConfigureFunc{func(s *smtp.Server) {
		s.MaxMessageBytes = limit
	}}, fn...)...)
	c.SetDeadline(time.Now().Add(10 * time.Second))

	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("Invalid MAIL response:", scanner.Text())
	}
	io.WriteString(c, "RCPT TO:<root@gchq.gov.uk>\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("Invalid RCPT response:", scanner.Text())
	}
	return be, s, c, scanner
}
