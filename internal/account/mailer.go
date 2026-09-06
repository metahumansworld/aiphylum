package account

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// SMTPMailer mails the sign-in link through one relay: PLAIN auth over
// STARTTLS, the shape of every submission relay on port 587, and no TLS at
// all for a local relay that offers none. Site is the public origin the
// link opens.
// ponytail: no implicit-TLS (port 465) and no retries; a relay that needs
// either gets its own Mailer behind the same interface.
type SMTPMailer struct {
	Addr       string // relay "host:port"
	From       string
	User, Pass string // optional: a local relay may ask no login
	Site       string
	Log        *slog.Logger
}

func (m *SMTPMailer) Send(_ context.Context, to, token string) error {
	// From may carry a display name — `soscitea <hi@...>` — which belongs in
	// the header but not in the envelope: MAIL FROM takes the bare address.
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return fmt.Errorf("mail from %q: %w", m.From, err)
	}
	conn, err := net.DialTimeout("tcp", m.Addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	// One deadline covers the whole exchange. smtp.SendMail has none, and a
	// stuck relay would hold a request handler open forever.
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	host, _, _ := net.SplitHostPort(m.Addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("greet relay: %w", err)
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if m.User != "" {
		// PlainAuth itself refuses to speak over plaintext except to
		// localhost, so a relay that skipped STARTTLS cannot leak the pass.
		if err := c.Auth(smtp.PlainAuth("", m.User, m.Pass, host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(signInMail(m.From, to, m.Site, token)); err != nil {
		return fmt.Errorf("write mail: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("send mail: %w", err)
	}
	m.Log.Info("sign-in link mailed", "to", to)
	return c.Quit()
}

// signInMail is the message, CRLF-joined as SMTP wants. The address came
// through normalizeEmail — no whitespace — so it cannot smuggle headers.
func signInMail(from, to, site, token string) []byte {
	link := strings.TrimRight(site, "/") + "/?token=" + token
	return []byte(strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: Your soscitea sign-in link",
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Open this link to sign in to soscitea:",
		"",
		link,
		"",
		// The copy cites defaultLinkTTL; a Config.LinkTTL override would
		// make it lie, and serve.go never overrides it.
		fmt.Sprintf("The link works once and expires in %d minutes. If you did not ask", int(defaultLinkTTL.Minutes())),
		"for it, ignore this mail and nothing happens.",
		"",
	}, "\r\n"))
}
