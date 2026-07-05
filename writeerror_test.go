package smtp

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// Once a response write fails, the serve loop must stop processing buffered
// commands: the client can no longer observe replies, so backend calls would
// run with unobservable outcomes.
func TestServer_WriteFailureStopsProcessing(t *testing.T) {
	backend := &chanBackend{
		data:   make(chan []byte, 1),
		mailed: make(chan string, 1),
	}
	server := NewServer(backend)
	server.Domain = "example.com"

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	done := make(chan error, 1)
	go func() {
		done <- server.handleConn(newConn(serverConn, server))
	}()

	clientConn.SetDeadline(time.Now().Add(10 * time.Second))
	client := bufio.NewReader(clientConn)

	if greeting, err := readLine(client); err != nil || !strings.HasPrefix(greeting, "220") {
		t.Fatalf("Invalid greeting: %q, %v", greeting, err)
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

	// Three pipelined commands in one write. The client reads only the first
	// response and disconnects, so writing the response to the second command
	// fails — and the third command must then never reach the backend.
	clientConn.Write([]byte("NOOP\r\nNOOP\r\nMAIL FROM:<sender@example.com>\r\n"))
	if line, err := readLine(client); err != nil || !strings.HasPrefix(line, "250 ") {
		t.Fatalf("Invalid NOOP response: %q, %v", line, err)
	}
	clientConn.Close()

	select {
	case err := <-done:
		// A peer disconnect is routine and must not be reported as an error.
		if err != nil {
			t.Fatal("Expected a silent close on peer disconnect, got:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Server did not end the connection after the write failure")
	}

	select {
	case from := <-backend.mailed:
		t.Fatal("Backend received MAIL after the response write failed:", from)
	default:
	}
}
