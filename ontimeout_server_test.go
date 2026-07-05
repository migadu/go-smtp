package smtp_test

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// TestServer_OnTimeoutHookIdle verifies that OnTimeout is invoked exactly once
// when the read deadline expires between commands, after the 421 notice is
// written — embedders count timeout disconnects from it, so a double
// invocation would skew their metrics.
func TestServer_OnTimeoutHookIdle(t *testing.T) {
	var timeouts atomic.Int32
	_, s, c, scanner := testServerGreeted(t, func(s *smtp.Server) {
		s.ReadTimeout = 200 * time.Millisecond
		s.OnTimeout = func() { timeouts.Add(1) }
	})
	defer s.Close()
	defer c.Close()

	// Stay silent past the read deadline; the server must send the idle 421.
	if !scanner.Scan() {
		t.Fatalf("expected 421 idle timeout notice, got read error: %v", scanner.Err())
	}
	line := scanner.Text()
	if !strings.HasPrefix(line, "421") || !strings.Contains(line, "Idle timeout") {
		t.Fatalf("expected '421 ... Idle timeout' notice, got %q", line)
	}

	// The connection must be closed with nothing else buffered.
	if scanner.Scan() {
		t.Fatalf("expected connection close after the notice, read %q", scanner.Text())
	}

	// Allow the serve goroutine to run the hook before asserting.
	deadline := time.Now().Add(2 * time.Second)
	for timeouts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := timeouts.Load(); got != 1 {
		t.Fatalf("expected exactly one OnTimeout invocation, got %d", got)
	}
}
