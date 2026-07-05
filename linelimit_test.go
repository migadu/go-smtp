package smtp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// Backend that hands received message payloads to the test over a channel,
// so the test never reads session state that the server goroutine mutates.
type chanBackend struct {
	data chan []byte

	// If non-nil, receives the sender address of every accepted MAIL command.
	mailed chan string
}

func (b *chanBackend) NewSession(*Conn) (Session, error) {
	return &chanSession{backend: b}, nil
}

type chanSession struct {
	backend *chanBackend
}

func (*chanSession) Reset()        {}
func (*chanSession) Logout() error { return nil }
func (s *chanSession) Mail(from string, opts *MailOptions) error {
	if s.backend.mailed != nil {
		s.backend.mailed <- from
	}
	return nil
}
func (*chanSession) Rcpt(to string, opts *RcptOptions) error {
	return nil
}
func (s *chanSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.backend.data <- data
	return nil
}

// A client may pipeline a BDAT command and its chunk payload into a single
// TCP segment (RFC 3030 explicitly allows sending BDAT commands without
// waiting for a response). When the payload contains a span longer than
// MaxLineLength without a newline — normal for BINARYMIME content — the
// payload bytes pulled into the buffer while reading the *command line* must
// not count toward the command's line length, or the server kills a
// legitimate transfer with "500 Too long line".
//
// net.Pipe is used instead of a TCP socket because the failure mode depends
// on the payload arriving in the same read as the command; TCP segment
// coalescing would make the test timing-dependent.
func TestServer_BDATPipelinedLongBinaryChunk(t *testing.T) {
	backend := &chanBackend{data: make(chan []byte, 1)}
	server := NewServer(backend)
	server.Domain = "example.com"

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() {
		server.handleConn(newConn(serverConn, server))
	}()

	clientConn.SetDeadline(time.Now().Add(10 * time.Second))
	client := bufio.NewReader(clientConn)

	greeting, err := readLine(client)
	if err != nil {
		t.Fatalf("Failed to read greeting: %v", err)
	}
	if !strings.HasPrefix(greeting, "220") {
		t.Fatalf("Expected 220 greeting, got: %s", greeting)
	}

	clientConn.Write([]byte("EHLO client.example.com\r\n"))
	for {
		line, err := readLine(client)
		if err != nil {
			t.Fatalf("Failed to read EHLO response: %v", err)
		}
		if strings.HasPrefix(line, "250 ") {
			break
		}
	}

	clientConn.Write([]byte("MAIL FROM:<sender@example.com>\r\n"))
	if line, err := readLine(client); err != nil || !strings.HasPrefix(line, "250 ") {
		t.Fatalf("Invalid MAIL response: %q, %v", line, err)
	}

	clientConn.Write([]byte("RCPT TO:<rcpt@example.com>\r\n"))
	if line, err := readLine(client); err != nil || !strings.HasPrefix(line, "250 ") {
		t.Fatalf("Invalid RCPT response: %q, %v", line, err)
	}

	// Newline-free payload longer than MaxLineLength (default 2000), sent in
	// ONE write together with the command so both arrive in the same read.
	payload := bytes.Repeat([]byte{0xff}, 2500)
	msg := append([]byte(fmt.Sprintf("BDAT %d LAST\r\n", len(payload))), payload...)
	if _, err := clientConn.Write(msg); err != nil {
		t.Fatalf("Failed to write pipelined BDAT: %v", err)
	}

	line, err := readLine(client)
	if err != nil {
		t.Fatalf("Failed to read BDAT response: %v", err)
	}
	if !strings.HasPrefix(line, "250 ") {
		t.Fatalf("Pipelined BDAT chunk was rejected: %s", line)
	}

	select {
	case data := <-backend.data:
		if !bytes.Equal(data, payload) {
			t.Fatalf("Invalid message payload: got %d bytes, want %d", len(data), len(payload))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Backend never received the message")
	}
}
