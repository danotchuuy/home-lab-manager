package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// sender delivers a message to one kind of target.
type sender interface {
	send(ctx context.Context, t Target, m message) error
}

// permanentError is a failure that retrying won't fix, like a rejected
// token.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// sortedFields returns an event's fields in a stable order.
func sortedFields(f map[string]string) [][2]string {
	out := make([][2]string, 0, len(f))
	for k, v := range f {
		out = append(out, [2]string{k, v})
	}
	slices.SortFunc(out, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// postJSON sends body to u and checks the response.
func postJSON(ctx context.Context, c *http.Client, u string, body any, headers http.Header) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "home-lab-manager")
	resp, err := c.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			// The URL may hold a token; report only what went wrong.
			return fmt.Errorf("%s %s: %w", uerr.Op, urlHint(u), uerr.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err = fmt.Errorf("%s answered %s: %s", urlHint(u), resp.Status, strings.TrimSpace(string(msg)))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests &&
		resp.StatusCode != http.StatusRequestTimeout {
		return &permanentError{err}
	}
	return err
}

// --- Discord ---

type discordSender struct{ c *http.Client }

var discordColor = map[Severity]int{Info: 0x3b82f6, Warning: 0xf59e0b, Error: 0xef4444}

func (d discordSender) send(ctx context.Context, t Target, m message) error {
	embed := map[string]any{
		"title":       truncate(m.Title, 250),
		"description": truncate(m.Message, 4000),
		"color":       discordColor[m.Severity],
		"timestamp":   m.Time.UTC().Format(time.RFC3339),
		"footer":      map[string]string{"text": "Home Lab Manager · " + m.Kind},
	}
	if m.URL != "" {
		embed["url"] = m.URL
	}
	var fields []map[string]any
	if m.Server != "" {
		fields = append(fields, map[string]any{"name": "Server", "value": m.Server, "inline": true})
	}
	for _, f := range sortedFields(m.Fields) {
		if len(fields) >= 20 {
			break
		}
		fields = append(fields, map[string]any{"name": truncate(f[0], 250), "value": truncate(f[1], 1000), "inline": true})
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}
	body := map[string]any{"username": "Home Lab Manager", "embeds": []any{embed}}
	return postJSON(ctx, d.c, t.URL, body, nil)
}

// --- ntfy ---

type ntfySender struct{ c *http.Client }

var ntfyPriority = map[Severity]int{Info: 3, Warning: 4, Error: 5}
var ntfyTag = map[Severity]string{Info: "information_source", Warning: "warning", Error: "rotating_light"}

func (n ntfySender) send(ctx context.Context, t Target, m message) error {
	body := map[string]any{
		"topic":    t.Topic,
		"title":    m.Title,
		"message":  messageText(m, false),
		"priority": ntfyPriority[m.Severity],
		"tags":     []string{ntfyTag[m.Severity]},
	}
	if m.URL != "" {
		body["click"] = m.URL
	}
	h := http.Header{}
	if t.Token != "" {
		h.Set("Authorization", "Bearer "+t.Token)
	}
	// Publishing as JSON goes to the server's root, with the topic in the
	// body.
	return postJSON(ctx, n.c, strings.TrimRight(t.URL, "/")+"/", body, h)
}

// --- Gotify ---

type gotifySender struct{ c *http.Client }

var gotifyPriority = map[Severity]int{Info: 4, Warning: 6, Error: 8}

func (g gotifySender) send(ctx context.Context, t Target, m message) error {
	body := map[string]any{
		"title":    m.Title,
		"message":  messageText(m, false),
		"priority": gotifyPriority[m.Severity],
	}
	if m.URL != "" {
		body["extras"] = map[string]any{"client::notification": map[string]any{"click": map[string]string{"url": m.URL}}}
	}
	h := http.Header{}
	h.Set("X-Gotify-Key", t.Token)
	return postJSON(ctx, g.c, strings.TrimRight(t.URL, "/")+"/message", body, h)
}

// --- Generic webhook ---

type webhookSender struct{ c *http.Client }

// WebhookBody is what the generic webhook receives.
type WebhookBody struct {
	Kind     string            `json:"kind"`
	Severity Severity          `json:"severity"`
	Title    string            `json:"title"`
	Message  string            `json:"message"`
	Server   string            `json:"server,omitempty"`
	Link     string            `json:"link,omitempty"`
	Time     time.Time         `json:"time"`
	Fields   map[string]string `json:"fields"`
}

func (w webhookSender) send(ctx context.Context, t Target, m message) error {
	body := WebhookBody{
		Kind: m.Kind, Severity: m.Severity, Title: m.Title, Message: m.Message, Server: m.Server,
		Link: m.URL, Time: m.Time.UTC(), Fields: m.Fields,
	}
	if body.Fields == nil {
		body.Fields = map[string]string{}
	}
	h := http.Header{}
	for _, hd := range t.Headers {
		h.Set(hd.Name, hd.Value)
	}
	return postJSON(ctx, w.c, t.URL, body, h)
}

// --- Email ---

type emailSender struct{}

func (emailSender) send(ctx context.Context, t Target, m message) error {
	addr := net.JoinHostPort(t.SMTPHost, strconv.Itoa(t.SMTPPort))
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsConf := &tls.Config{ServerName: t.SMTPHost, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	var err error
	if t.Security == "tls" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, t.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if t.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return &permanentError{fmt.Errorf("%s doesn't offer STARTTLS; choose another security setting", addr)}
		}
		if err := c.StartTLS(tlsConf); err != nil {
			return err
		}
	}
	if t.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", t.Username, t.Password, t.SMTPHost)); err != nil {
			return &permanentError{fmt.Errorf("sign in to %s: %w", addr, err)}
		}
	}
	from := t.From
	if a, err := parseAddress(from); err == nil {
		from = a
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, to := range t.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(emailBody(t, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// messageText is the message, the server and fields, and (unless the
// target shows it separately) the link, as plain text.
func messageText(m message, withLink bool) string {
	var b strings.Builder
	b.WriteString(m.Message)
	if m.Server != "" {
		fmt.Fprintf(&b, "\nServer: %s", m.Server)
	}
	for _, f := range sortedFields(m.Fields) {
		fmt.Fprintf(&b, "\n%s: %s", f[0], f[1])
	}
	if withLink && m.URL != "" {
		fmt.Fprintf(&b, "\n\n%s", m.URL)
	}
	return strings.TrimSpace(b.String())
}

func emailBody(t Target, m message) []byte {
	var b bytes.Buffer
	boundary := "hlm-" + newID()
	subject := fmt.Sprintf("[hlm] %s", m.Title)
	if m.Severity == Error {
		subject = fmt.Sprintf("[hlm] ⚠ %s", m.Title)
	}
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("From", t.From)
	hdr("To", strings.Join(t.To, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", subject))
	hdr("Date", m.Time.Format(time.RFC1123Z))
	hdr("Message-ID", fmt.Sprintf("<%s@home-lab-manager>", newID()))
	hdr("MIME-Version", "1.0")
	hdr("Auto-Submitted", "auto-generated")
	hdr("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")

	part := func(contentType, body string) {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary, contentType)
		qp := quotedprintable.NewWriter(&b)
		_, _ = qp.Write([]byte(body))
		_ = qp.Close()
		b.WriteString("\r\n")
	}
	part("text/plain", messageText(m, true)+"\n\n— Home Lab Manager ("+m.Kind+")\n")

	var h strings.Builder
	color := map[Severity]string{Info: "#3b82f6", Warning: "#f59e0b", Error: "#ef4444"}[m.Severity]
	fmt.Fprintf(&h, `<div style="font-family:sans-serif;border-left:4px solid %s;padding-left:12px">`, color)
	fmt.Fprintf(&h, "<h2 style=\"margin:0 0 8px\">%s</h2>", html.EscapeString(m.Title))
	if m.Message != "" {
		fmt.Fprintf(&h, "<p style=\"white-space:pre-wrap\">%s</p>", html.EscapeString(m.Message))
	}
	if m.Server != "" || len(m.Fields) > 0 {
		h.WriteString("<table>")
		if m.Server != "" {
			fmt.Fprintf(&h, "<tr><td><b>Server</b></td><td>%s</td></tr>", html.EscapeString(m.Server))
		}
		for _, f := range sortedFields(m.Fields) {
			fmt.Fprintf(&h, "<tr><td><b>%s</b></td><td>%s</td></tr>", html.EscapeString(f[0]), html.EscapeString(f[1]))
		}
		h.WriteString("</table>")
	}
	if m.URL != "" {
		fmt.Fprintf(&h, `<p><a href="%s">Open in Home Lab Manager</a></p>`, html.EscapeString(m.URL))
	}
	fmt.Fprintf(&h, `<p style="color:#888;font-size:12px">Home Lab Manager · %s</p></div>`, html.EscapeString(m.Kind))
	part("text/html", h.String())
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// parseAddress returns the bare address in an address like
// "hlm <hlm@example.com>".
func parseAddress(s string) (string, error) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return "", err
	}
	return a.Address, nil
}
