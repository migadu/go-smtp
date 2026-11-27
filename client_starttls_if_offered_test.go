package smtp

import "testing"

func TestNewClientStartTLSIfOfferedKeepsPlaintextSession(t *testing.T) {
	for _, tc := range []struct {
		name, greeting, commands, mailParams string
	}{
		{
			name:       "EHLO without STARTTLS",
			greeting:   "250-mx.example\n250 8BITMIME\n",
			commands:   "EHLO sender.example\n",
			mailParams: " BODY=8BITMIME",
		},
		{
			name:     "HELO fallback",
			greeting: "502 unknown command\n250 mx.example\n",
			commands: "EHLO sender.example\nHELO sender.example\n",
		},
		{
			name:     "non-ASCII keyword look-alike",
			greeting: "250-mx.example\n250 STARTTLſ\n",
			commands: "EHLO sender.example\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newStartTLSScriptConn("220 hello\n" + tc.greeting + "250 sender ok\n250 recipient ok\n221 bye\n")
			c, started, err := NewClientStartTLSIfOfferedWithName(conn, nil, "sender.example")
			if err != nil || c == nil || started {
				t.Fatalf("constructor = %v, %v, %v; want a plaintext client", c, started, err)
			}
			defer c.Close()
			if conn.closed {
				t.Fatal("constructor closed the connection")
			}
			if _, ok := c.TLSConnectionState(); ok {
				t.Fatal("plaintext client reports a TLS connection")
			}
			if err := c.Mail("a@sender.example", nil); err != nil {
				t.Fatalf("Mail: %v", err)
			}
			if err := c.Rcpt("b@rcpt.example", nil); err != nil {
				t.Fatalf("Rcpt: %v", err)
			}
			if err := c.Quit(); err != nil {
				t.Fatalf("Quit: %v", err)
			}
			want := tc.commands + "MAIL FROM:<a@sender.example>" + tc.mailParams + "\nRCPT TO:<b@rcpt.example>\nQUIT\n"
			if got := conn.commands(); got != want {
				t.Errorf("client wrote %q, want %q", got, want)
			}
		})
	}
}

func TestNewClientStartTLSIfOfferedStartsHandshakeOnNextIO(t *testing.T) {
	conn := newStartTLSScriptConn("220 hello\n250-mx.example\n250 StartTLS\n220 go ahead\n")
	c, started, err := NewClientStartTLSIfOfferedWithName(conn, nil, "sender.example")
	if err != nil || c == nil || !started {
		t.Fatalf("constructor = %v, %v, %v; want a STARTTLS client", c, started, err)
	}
	defer c.Close()
	if got, want := conn.commands(), "EHLO sender.example\nSTARTTLS\n"; got != want {
		t.Errorf("client wrote %q before handshake, want %q", got, want)
	}
	if state, ok := c.TLSConnectionState(); !ok || state.HandshakeComplete {
		t.Fatalf("TLSConnectionState = %+v, %v; want a TLS connection awaiting its handshake", state, ok)
	}
	// The scripted peer provides no TLS handshake response.
	if err := c.Hello("sender.example"); err == nil {
		t.Fatal("Hello succeeded when the TLS handshake could not complete")
	}
	if state, _ := c.TLSConnectionState(); state.HandshakeComplete {
		t.Error("handshake reported complete after failure")
	}
}

func TestNewClientStartTLSIfOfferedSetupErrorsMatchStrictConstructor(t *testing.T) {
	for _, tc := range []struct {
		name, localName, replies string
	}{
		{"invalid local name", "sender\n.example", "220 hello\n"},
		{"greeting refused", "sender.example", "554 denied\n221 bye\n"},
		{"EHLO refused", "sender.example", "220 hello\n550 denied\n221 bye\n"},
		{"EHLO channel closed", "sender.example", "220 hello\n421 closing\n"},
		{"STARTTLS refused", "sender.example", "220 hello\n250-mx.example\n250 STARTTLS\n454 unavailable\n221 bye\n"},
		{"STARTTLS channel closed", "sender.example", "220 hello\n250-mx.example\n250 STARTTLS\n421 closing\n"},
		{"unexpected STARTTLS reply", "sender.example", "220 hello\n250-mx.example\n250 STARTTLS\n250 not 220\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			strictConn := newStartTLSScriptConn(tc.replies)
			strict, strictErr := NewClientStartTLSWithName(strictConn, nil, tc.localName)
			if strictErr == nil || strict != nil || !strictConn.closed {
				t.Fatalf("strict constructor = %v, %v, closed = %v; want an error and a closed connection", strict, strictErr, strictConn.closed)
			}
			conn := newStartTLSScriptConn(tc.replies)
			c, started, err := NewClientStartTLSIfOfferedWithName(conn, nil, tc.localName)
			if err == nil || c != nil || started || !conn.closed {
				t.Fatalf("constructor = %v, %v, %v, closed = %v; want an error and a closed connection", c, started, err, conn.closed)
			}
			if got, want := err.Error(), strictErr.Error(); got != want {
				t.Errorf("constructor error = %q, strict error = %q", got, want)
			}
			if got, want := conn.commands(), strictConn.commands(); got != want {
				t.Errorf("client wrote %q, strict client wrote %q", got, want)
			}
		})
	}
}
