package smtp_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// generateTestCert builds an ephemeral self-signed certificate for STARTTLS
// tests. The client side uses InsecureSkipVerify, so identity details only need
// to be syntactically valid.
func generateTestCert() tls.Certificate {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// withTLS configures the server with an ephemeral certificate so STARTTLS can
// be exercised.
func withTLS(s *smtp.Server) {
	s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{generateTestCert()}}
}

// readResponseLine reads a single SMTP response line, failing on error/EOF.
func readResponseLine(t *testing.T, scanner *bufio.Scanner) string {
	t.Helper()
	if !scanner.Scan() {
		t.Fatalf("expected a response line, got none (err=%v)", scanner.Err())
	}
	return scanner.Text()
}

// readEhloCaps consumes a multiline EHLO response and returns the advertised
// capabilities as a set.
func readEhloCaps(t *testing.T, scanner *bufio.Scanner) map[string]bool {
	t.Helper()
	caps := map[string]bool{}
	first := readResponseLine(t, scanner)
	if !strings.HasPrefix(first, "250") {
		t.Fatalf("unexpected EHLO response: %q", first)
	}
	if strings.HasPrefix(first, "250 ") {
		return caps
	}
	for {
		line := readResponseLine(t, scanner)
		if strings.HasPrefix(line, "250 ") {
			caps[strings.TrimPrefix(line, "250 ")] = true
			break
		}
		if !strings.HasPrefix(line, "250-") {
			t.Fatalf("unexpected capability line: %q", line)
		}
		caps[strings.TrimPrefix(line, "250-")] = true
	}
	return caps
}

// startTLS drives a successful STARTTLS upgrade from the client side and
// returns the established *tls.Conn together with a scanner over it.
func startTLS(t *testing.T, c net.Conn, scanner *bufio.Scanner) (*tls.Conn, *bufio.Scanner) {
	t.Helper()
	if _, err := c.Write([]byte("STARTTLS\r\n")); err != nil {
		t.Fatal(err)
	}
	if resp := readResponseLine(t, scanner); !strings.HasPrefix(resp, "220 ") {
		t.Fatalf("expected 220 ready, got %q", resp)
	}
	tlsConn := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client handshake failed: %v", err)
	}
	return tlsConn, bufio.NewScanner(tlsConn)
}

func TestServer_STARTTLS_advertisedAndUpgrades(t *testing.T) {
	be, s, c, scanner, caps := testServerEhlo(t, withTLS)
	defer s.Close()
	defer c.Close()

	if !caps["STARTTLS"] {
		t.Fatal("STARTTLS capability not advertised over plaintext")
	}

	tlsConn, tlsScanner := startTLS(t, c, scanner)

	// After the upgrade STARTTLS must no longer be advertised.
	if _, err := tlsConn.Write([]byte("EHLO localhost\r\n")); err != nil {
		t.Fatal(err)
	}
	tlsCaps := readEhloCaps(t, tlsScanner)
	if tlsCaps["STARTTLS"] {
		t.Fatal("STARTTLS still advertised after TLS upgrade")
	}

	// The pre-TLS session must have been logged out and a fresh one created,
	// per RFC 3207 section 4.2 (discard pre-TLS knowledge).
	be.mu.Lock()
	nSessions := len(be.sessions)
	firstLogouts := be.sessions[0].getLogoutCount()
	be.mu.Unlock()
	if nSessions < 2 {
		t.Fatalf("expected a new session after STARTTLS, got %d total", nSessions)
	}
	if firstLogouts < 1 {
		t.Fatal("pre-TLS session was not logged out on STARTTLS")
	}
}

// TestServer_STARTTLS_plaintextInjection is the regression test for the
// STARTTLS plaintext command injection attack: any bytes pipelined after the
// STARTTLS command (before the TLS handshake) must never be executed. The
// server must detect the buffered plaintext and abort the connection.
func TestServer_STARTTLS_plaintextInjection(t *testing.T) {
	_, s, c, scanner, _ := testServerEhlo(t, withTLS)
	defer s.Close()
	defer c.Close()

	// Pipeline an injected command in the same write as STARTTLS, before the
	// TLS handshake. A MITM would use this to smuggle a command that appears
	// to originate from inside the TLS tunnel.
	if _, err := c.Write([]byte("STARTTLS\r\nMAIL FROM:<injected@attacker.example>\r\n")); err != nil {
		t.Fatal(err)
	}

	if resp := readResponseLine(t, scanner); !strings.HasPrefix(resp, "220 ") {
		t.Fatalf("expected 220 ready, got %q", resp)
	}

	// The server must close the connection rather than processing the injected
	// command. The injected MAIL must never produce a response.
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "250") || strings.Contains(line, "Ok") || strings.Contains(line, "OK") {
			t.Fatalf("injected command was processed: %q", line)
		}
	}
	// scanner stopped: the connection was closed (expected).
}

func TestServer_STARTTLS_rejectedWhenAlreadyTLS(t *testing.T) {
	_, s, c, scanner, _ := testServerEhlo(t, withTLS)
	defer s.Close()
	defer c.Close()

	tlsConn, tlsScanner := startTLS(t, c, scanner)

	if _, err := tlsConn.Write([]byte("STARTTLS\r\n")); err != nil {
		t.Fatal(err)
	}
	resp := readResponseLine(t, tlsScanner)
	if !strings.HasPrefix(resp, "502") {
		t.Fatalf("expected 502 when already in TLS, got %q", resp)
	}
}

func TestServer_STARTTLS_notSupportedWithoutConfig(t *testing.T) {
	_, s, c, scanner, caps := testServerEhlo(t)
	defer s.Close()
	defer c.Close()

	if caps["STARTTLS"] {
		t.Fatal("STARTTLS advertised without a TLSConfig")
	}

	if _, err := c.Write([]byte("STARTTLS\r\n")); err != nil {
		t.Fatal(err)
	}
	resp := readResponseLine(t, scanner)
	if !strings.HasPrefix(resp, "502") {
		t.Fatalf("expected 502 TLS not supported, got %q", resp)
	}
}

// TestServer_STARTTLS_resetsTransaction verifies that a transaction started
// before STARTTLS is discarded after the upgrade.
func TestServer_STARTTLS_resetsTransaction(t *testing.T) {
	_, s, c, scanner, _ := testServerEhlo(t, withTLS)
	defer s.Close()
	defer c.Close()

	if _, err := c.Write([]byte("MAIL FROM:<sender@example.com>\r\n")); err != nil {
		t.Fatal(err)
	}
	if resp := readResponseLine(t, scanner); !strings.HasPrefix(resp, "250") {
		t.Fatalf("expected 250 for MAIL, got %q", resp)
	}

	tlsConn, tlsScanner := startTLS(t, c, scanner)

	// EHLO again over TLS, then DATA without a fresh MAIL: the pre-TLS
	// MAIL FROM must have been discarded, so DATA must be rejected.
	if _, err := tlsConn.Write([]byte("EHLO localhost\r\n")); err != nil {
		t.Fatal(err)
	}
	readEhloCaps(t, tlsScanner)

	if _, err := tlsConn.Write([]byte("DATA\r\n")); err != nil {
		t.Fatal(err)
	}
	resp := readResponseLine(t, tlsScanner)
	if !strings.HasPrefix(resp, "502") {
		t.Fatalf("expected 502 (transaction reset) after STARTTLS, got %q", resp)
	}
}

// TestServer_STARTTLS_handshakeFailureClosesConn verifies that a failed TLS
// handshake tears the connection down instead of leaving it desynchronized.
func TestServer_STARTTLS_handshakeFailureClosesConn(t *testing.T) {
	_, s, c, scanner, _ := testServerEhlo(t, withTLS)
	defer s.Close()
	defer c.Close()

	if _, err := c.Write([]byte("STARTTLS\r\n")); err != nil {
		t.Fatal(err)
	}
	if resp := readResponseLine(t, scanner); !strings.HasPrefix(resp, "220 ") {
		t.Fatalf("expected 220 ready, got %q", resp)
	}

	// Send bytes that are not a valid TLS ClientHello (sent only after the 220
	// so they are not buffered with the STARTTLS line and reach the handshake).
	if _, err := c.Write([]byte("this is not a TLS handshake\r\n")); err != nil {
		t.Fatal(err)
	}

	// The server must close the connection. It must not fall back to reading
	// the bogus bytes as a plaintext SMTP command and emit an SMTP response.
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	for {
		n, err := c.Read(buf)
		if err != nil {
			break // connection closed, as expected
		}
		if strings.Contains(string(buf[:n]), "250 ") {
			t.Fatalf("server processed post-handshake-failure plaintext: %q", string(buf[:n]))
		}
	}
}

// TestServer_STARTTLS_handshakeRespectsReadTimeout verifies that a client which
// requests STARTTLS but never drives the handshake does not pin a goroutine:
// the configured ReadTimeout must apply to the handshake.
func TestServer_STARTTLS_handshakeRespectsReadTimeout(t *testing.T) {
	_, s, c, scanner, _ := testServerEhlo(t, withTLS, func(s *smtp.Server) {
		s.ReadTimeout = 500 * time.Millisecond
		s.WriteTimeout = 500 * time.Millisecond
	})
	defer s.Close()
	defer c.Close()

	if _, err := c.Write([]byte("STARTTLS\r\n")); err != nil {
		t.Fatal(err)
	}
	if resp := readResponseLine(t, scanner); !strings.HasPrefix(resp, "220 ") {
		t.Fatalf("expected 220 ready, got %q", resp)
	}

	// Do not perform the TLS handshake. The server's handshake read must time
	// out and close the connection well within this generous deadline.
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("expected connection to be closed after handshake read timeout")
	}
}

// TestServer_STARTTLS_upgradeRaceWithClose stresses the connection swap inside
// handleStartTLS against Server.Close(), which calls Conn.Close() on every
// tracked connection from a different goroutine. The swap (c.conn = tlsConn)
// and the close-side read of c.conn must be synchronized; otherwise this trips
// the race detector. Run with -race for it to be meaningful.
func TestServer_STARTTLS_upgradeRaceWithClose(t *testing.T) {
	const rounds = 6
	const conns = 32

	for round := 0; round < rounds; round++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		s := smtp.NewServer(new(backend))
		s.Domain = "localhost"
		s.AllowInsecureAuth = true
		// Keep stalled handshakes from lingering past the test.
		s.ReadTimeout = 2 * time.Second
		s.WriteTimeout = 2 * time.Second
		withTLS(s)
		go s.Serve(l)

		addr := l.Addr().String()
		var wg sync.WaitGroup
		wg.Add(conns)
		for i := 0; i < conns; i++ {
			go func() {
				defer wg.Done()
				driveStartTLS(addr)
			}()
		}

		// Close the server while the STARTTLS upgrades are in flight, so the
		// per-connection c.conn swap races Server.Close()'s Conn.Close() reads.
		time.Sleep(time.Duration(round) * time.Millisecond)
		s.Close()
		wg.Wait()
		l.Close()
	}
}

// driveStartTLS performs an EHLO + STARTTLS + TLS handshake against addr,
// tolerating errors at any point (the server may close mid-flight). It exists
// to exercise the upgrade path concurrently; it makes no assertions.
func driveStartTLS(addr string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	if _, err := r.ReadString('\n'); err != nil { // greeting
		return
	}
	if _, err := conn.Write([]byte("EHLO localhost\r\n")); err != nil {
		return
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(line, "250 ") {
			break
		}
	}
	if _, err := conn.Write([]byte("STARTTLS\r\n")); err != nil {
		return
	}
	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "220") {
		return
	}
	// r has consumed nothing past the 220 (the server stays silent until the
	// handshake), so reading the raw conn from here is safe.
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	_ = tlsConn.Handshake()
}
