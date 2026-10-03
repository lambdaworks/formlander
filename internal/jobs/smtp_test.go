package jobs

import (
	"bufio"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"formlander/internal/integrations"
)

// capturedMail records what a fake SMTP server received.
type capturedMail struct {
	mu           sync.Mutex
	from         string
	to           string
	data         string
	greeting     string
	authReceived bool
}

// startFakeSMTPServer spins up a minimal plaintext SMTP server on a random
// loopback port for one connection, capturing the envelope and message.
func startFakeSMTPServer(t *testing.T, rejectLocalhost ...bool) (host string, port int, captured *capturedMail) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	captured = &capturedMail{}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		r := bufio.NewReader(conn)
		write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }

		write("220 fake ESMTP")
		inData := false
		var dataLines []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")

			if inData {
				if line == "." {
					captured.mu.Lock()
					captured.data = strings.Join(dataLines, "\n")
					captured.mu.Unlock()
					inData = false
					write("250 OK queued")
					continue
				}
				dataLines = append(dataLines, line)
				continue
			}

			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				captured.mu.Lock()
				captured.greeting = line
				captured.mu.Unlock()
				if len(rejectLocalhost) > 0 && rejectLocalhost[0] && strings.HasSuffix(line, " localhost") {
					write("421 4.7.0 closing connection (EHLO)")
					return
				}
				write("250-fake greets you")
				write("250 AUTH PLAIN LOGIN")
			case strings.HasPrefix(line, "AUTH"):
				captured.mu.Lock()
				captured.authReceived = true
				captured.mu.Unlock()
				write("235 2.7.0 Authentication successful")
			case strings.HasPrefix(line, "MAIL FROM:"):
				captured.mu.Lock()
				captured.from = line
				captured.mu.Unlock()
				write("250 OK")
			case strings.HasPrefix(line, "RCPT TO:"):
				captured.mu.Lock()
				captured.to = line
				captured.mu.Unlock()
				write("250 OK")
			case strings.HasPrefix(line, "DATA"):
				write("354 End data with <CR><LF>.<CR><LF>")
				inData = true
			case strings.HasPrefix(line, "QUIT"):
				write("221 Bye")
				return
			default:
				write("250 OK")
			}
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port, captured
}

func TestSendSMTP(t *testing.T) {
	t.Run("delivers with PLAIN auth over plaintext", func(t *testing.T) {
		host, port, captured := startFakeSMTPServer(t)
		cfg := &smtpConfig{
			Host: host, Port: port,
			Username: "apikey", Password: "secret",
			Encryption: "none",
			From:       "Forms <forms@example.com>",
			To:         "owner@example.com",
		}
		msg := buildSMTPMessage(message{From: cfg.From, To: cfg.To, Subject: "New submission", Body: "name: Alice"})

		err := sendSMTP(cfg, msg)
		require.NoError(t, err)

		captured.mu.Lock()
		defer captured.mu.Unlock()
		assert.True(t, captured.authReceived, "expected AUTH command")
		assert.Contains(t, captured.from, "forms@example.com")
		assert.Contains(t, captured.to, "owner@example.com")
		assert.Contains(t, captured.data, "Subject: New submission")
		assert.Contains(t, captured.data, "name: Alice")
	})

	t.Run("skips auth when username empty", func(t *testing.T) {
		host, port, captured := startFakeSMTPServer(t)
		cfg := &smtpConfig{Host: host, Port: port, Encryption: "none", From: "a@x.com", To: "b@x.com"}

		err := sendSMTP(cfg, buildSMTPMessage(message{From: cfg.From, To: cfg.To, Subject: "S", Body: "B"}))
		require.NoError(t, err)

		captured.mu.Lock()
		defer captured.mu.Unlock()
		assert.False(t, captured.authReceived, "expected no AUTH when username empty")
		assert.Contains(t, captured.from, "a@x.com")
	})

	t.Run("uses the configured greeting with a relay that rejects localhost", func(t *testing.T) {
		t.Setenv("FORMLANDER_SMTP_HELO_HOSTNAME", "formlander.example.com")
		host, port, captured := startFakeSMTPServer(t, true)
		profile := &integrations.MailerProfile{SMTPHost: host, SMTPPort: port, SMTPEncryption: "none"}
		cfg := smtpConfigFromProfile(profile, "forms@example.com", "owner@example.com")

		err := sendSMTP(cfg, buildSMTPMessage(message{From: cfg.From, To: cfg.To, Subject: "Test", Body: "Test"}))
		require.NoError(t, err)
		captured.mu.Lock()
		defer captured.mu.Unlock()
		assert.Equal(t, "EHLO formlander.example.com", captured.greeting)
		assert.False(t, captured.authReceived)
	})

	t.Run("preserves the default greeting without configuration", func(t *testing.T) {
		t.Setenv("FORMLANDER_SMTP_HELO_HOSTNAME", "")
		host, port, captured := startFakeSMTPServer(t)
		profile := &integrations.MailerProfile{SMTPHost: host, SMTPPort: port, SMTPEncryption: "none"}
		cfg := smtpConfigFromProfile(profile, "forms@example.com", "owner@example.com")
		require.NoError(t, sendSMTP(cfg, buildSMTPMessage(message{From: cfg.From, To: cfg.To, Body: "Test"})))
		captured.mu.Lock()
		defer captured.mu.Unlock()
		assert.Equal(t, "EHLO localhost", captured.greeting)
	})

	t.Run("identifies a rejected greeting in the error", func(t *testing.T) {
		host, port, _ := startFakeSMTPServer(t, true)
		cfg := &smtpConfig{Host: host, Port: port, Encryption: "none", HeloHostname: "localhost"}
		err := sendSMTP(cfg, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SMTP EHLO/HELO:")
	})

	t.Run("rejects an invalid greeting before connecting", func(t *testing.T) {
		cfg := &smtpConfig{Host: "invalid.example", Port: 587, HeloHostname: "example.com\r\nMAIL FROM:<a@example.com>"}
		err := sendSMTP(cfg, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SMTP HELO hostname:")
		assert.NotContains(t, err.Error(), "MAIL FROM")
	})

	t.Run("gives up when the server never sends a greeting", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			time.Sleep(2 * time.Second)
		}()
		previous := smtpTimeout
		smtpTimeout = 200 * time.Millisecond
		t.Cleanup(func() { smtpTimeout = previous })
		port := ln.Addr().(*net.TCPAddr).Port
		cfg := &smtpConfig{Host: "127.0.0.1", Port: port, Encryption: "none", From: "a@x.com", To: "b@x.com"}

		start := time.Now()
		err = sendSMTP(cfg, buildSMTPMessage(message{From: cfg.From, To: cfg.To, Subject: "S", Body: "B"}))

		require.Error(t, err)
		assert.Less(t, time.Since(start), time.Second)
	})
}

func TestBuildSMTPMessage(t *testing.T) {
	t.Run("includes RFC 5322 headers and body", func(t *testing.T) {
		msg := buildSMTPMessage(message{From: "Forms <forms@example.com>", To: "owner@example.com", Subject: "New submission", Body: "name: Alice\nemail: alice@x.com"})

		s := string(msg)
		if !strings.Contains(s, "From: Forms <forms@example.com>\r\n") {
			t.Errorf("missing From header, got:\n%s", s)
		}
		if !strings.Contains(s, "To: owner@example.com\r\n") {
			t.Errorf("missing To header, got:\n%s", s)
		}
		if !strings.Contains(s, "Subject: New submission\r\n") {
			t.Errorf("missing Subject header, got:\n%s", s)
		}
		if !strings.Contains(s, "MIME-Version: 1.0\r\n") {
			t.Errorf("missing MIME-Version header, got:\n%s", s)
		}
		if !strings.Contains(s, "Content-Type: text/plain; charset=\"utf-8\"\r\n") {
			t.Errorf("missing Content-Type header, got:\n%s", s)
		}
	})

	t.Run("separates headers from body with a blank CRLF line", func(t *testing.T) {
		msg := buildSMTPMessage(message{From: "a@x.com", To: "b@x.com", Subject: "Hi", Body: "line one\nline two"})

		s := string(msg)
		if !strings.Contains(s, "\r\n\r\n") {
			t.Errorf("expected blank line separating headers and body, got:\n%s", s)
		}
		// body newlines normalized to CRLF
		if !strings.Contains(s, "line one\r\nline two") {
			t.Errorf("expected body lines joined with CRLF, got:\n%s", s)
		}
	})

	t.Run("keeps a long subject outside ASCII below the line limit of SMTP", func(t *testing.T) {
		subject := strings.Repeat("é", 200)

		msg := buildSMTPMessage(message{From: "a@x.com", To: "b@x.com", Subject: subject, Body: "B"})

		for _, line := range strings.Split(string(msg), "\r\n") {
			assert.LessOrEqual(t, len(line), 998, "line: %s", line)
		}
		assert.Equal(t, subject, subjectOf(t, string(msg)))
	})

	t.Run("keeps a line break in the subject out of the headers", func(t *testing.T) {
		msg := buildSMTPMessage(message{From: "a@x.com", To: "b@x.com", Subject: "Hi\r\nBcc: thief@example.com", Body: "B"})

		parsed, err := mail.ReadMessage(strings.NewReader(string(msg)))
		require.NoError(t, err)
		assert.Empty(t, parsed.Header.Get("Bcc"))
	})
}
