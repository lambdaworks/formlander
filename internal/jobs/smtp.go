package jobs

import (
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// smtpTimeout bounds one SMTP send, from dial to QUIT.
var smtpTimeout = 30 * time.Second

// smtpConfig holds the resolved settings for one SMTP send.
type smtpConfig struct {
	Host         string
	Port         int
	Username     string
	Password     string
	Encryption   string // starttls | tls | none
	HeloHostname string // empty preserves Go's default localhost greeting
	From         string // header form, e.g. "Name <addr>"
	To           string
}

// sendSMTP delivers a pre-built message via SMTP. TLS modes:
//   - "tls":      implicit TLS from connect (typically port 465)
//   - "starttls": upgrade the plaintext connection (typically port 587)
//   - "none":     plaintext (port 25); auth is only allowed to localhost relays
//
// Certificates are always verified; credentials are never logged.
func sendSMTP(cfg *smtpConfig, msg []byte) error {
	if err := validateSMTPHeloHostname(cfg.HeloHostname); err != nil {
		return err
	}
	conn, err := dialSMTP(cfg)
	if err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTP server greeting: %w", err)
	}
	defer client.Close()

	// Set the greeting before STARTTLS; Go reuses it after the TLS upgrade.
	if cfg.HeloHostname != "" {
		if err := client.Hello(cfg.HeloHostname); err != nil {
			return fmt.Errorf("SMTP EHLO/HELO: %w", err)
		}
	}
	if cfg.Encryption == "starttls" {
		if err := client.StartTLS(tlsConfigFor(cfg.Host)); err != nil {
			return fmt.Errorf("SMTP STARTTLS: %w", err)
		}
	}
	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP AUTH: %w", err)
		}
	}
	return deliverSMTP(client, cfg, msg)
}

func dialSMTP(cfg *smtpConfig) (net.Conn, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: smtpTimeout}
	var conn net.Conn
	var err error
	if cfg.Encryption == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfigFor(cfg.Host))
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP connect: %w", err)
	}
	// One deadline covers the whole session, from greeting through QUIT.
	if err := conn.SetDeadline(time.Now().Add(smtpTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP deadline: %w", err)
	}
	return conn, nil
}

func deliverSMTP(client *smtp.Client, cfg *smtpConfig, msg []byte) error {
	if err := client.Mail(envelopeAddr(cfg.From)); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	if err := client.Rcpt(envelopeAddr(cfg.To)); err != nil {
		return fmt.Errorf("SMTP RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("SMTP message write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP message acceptance: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("SMTP QUIT after message acceptance: %w", err)
	}
	return nil
}

func validateSMTPHeloHostname(hostname string) error {
	if hostname == "" {
		return nil
	}
	if len(hostname) > 253 {
		return fmt.Errorf("SMTP HELO hostname: exceeds 253 characters")
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("SMTP HELO hostname: invalid DNS label")
		}
		for _, ch := range label {
			if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
				(ch >= '0' && ch <= '9') || ch == '-') {
				return fmt.Errorf("SMTP HELO hostname: invalid DNS character")
			}
		}
	}
	return nil
}

func tlsConfigFor(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// envelopeAddr extracts the bare address for MAIL FROM / RCPT TO, stripping any
// display name. Falls back to the trimmed input if it can't be parsed.
func envelopeAddr(s string) string {
	if addr, err := mail.ParseAddress(s); err == nil {
		return addr.Address
	}
	return strings.TrimSpace(s)
}

// message is one email, ready for a provider to send.
type message struct {
	From    string // header form, e.g. "Name <addr>"
	To      string
	ReplyTo string // empty for no Reply-To header
	Subject string
	Body    string
}

// subjectHeader returns a subject as the value of the Subject header. A
// subject outside ASCII goes out as RFC 2047 encoded words of 75 characters at
// most. Each word after the first goes on its own line, so a long subject
// stays below the 998 characters that SMTP allows on a line.
func subjectHeader(subject string) string {
	return strings.ReplaceAll(mime.QEncoding.Encode("utf-8", subject), "?= =?", "?=\r\n =?")
}

// buildSMTPMessage assembles a minimal RFC 5322 plain-text email message.
// Header and body line endings are normalized to CRLF as required by SMTP.
func buildSMTPMessage(m message) []byte {
	var b strings.Builder
	b.WriteString("From: " + m.From + "\r\n")
	b.WriteString("To: " + m.To + "\r\n")
	if m.ReplyTo != "" {
		b.WriteString("Reply-To: " + m.ReplyTo + "\r\n")
	}
	b.WriteString("Subject: " + subjectHeader(m.Subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(m.Body, "\n", "\r\n"))
	return []byte(b.String())
}
