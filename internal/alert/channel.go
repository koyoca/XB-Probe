package alert

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"
	"unicode"
)

// Message 是待投递的告警消息。
type Message struct {
	Title string
	Body  string
}

// Channel 是告警渠道（Telegram / SMTP）。
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// BuildMessage 生成告警文本，并对不可信字符串做基础清洗（13.4.4 / S1）。
func BuildMessage(level, nodeName, kind, detail string) string {
	return fmt.Sprintf("%s | %s | %s | %s",
		strings.ToUpper(level), stripControl(nodeName), stripControl(kind), stripControl(detail))
}

// stripControl 移除控制字符（保留常规空白），防止日志/消息注入。
func stripControl(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || r == '\r' {
			b.WriteByte(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// SanitizeHeader 移除 CR/LF，防止邮件头注入（13.4.4）。
func SanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// EscapeTelegramHTML 按 Telegram HTML 解析模式转义。
func EscapeTelegramHTML(s string) string { return html.EscapeString(stripControl(s)) }

// EscapeTelegramMarkdownV2 转义 MarkdownV2 保留字符。
func EscapeTelegramMarkdownV2(s string) string {
	const reserved = `_*[]()~` + "`" + `>#+-=|{}.!\`
	var b strings.Builder
	for _, r := range stripControl(s) {
		if strings.ContainsRune(reserved, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Telegram
// ---------------------------------------------------------------------------

// TelegramConfig 是 alert_channels.config_enc 解密后的内容。
type TelegramConfig struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"` // 可为 "chat_id" 或 "@channelname"
	// ParseMode 取 "HTML" 或 "MarkdownV2"（大小写不敏感）；留空按纯文本发送。
	//
	// 这个值会被**同时**用于两件事：选转义方式，以及作为 `parse_mode` 字段发给
	// Telegram。两者必须一致——只转义不声明的话，转义产物会被当成普通文本
	// 原样显示（用户看到字面的 `<b>` 和 `\*`）。
	ParseMode string `json:"parse_mode,omitempty"`
	// APIBase 可指向自建反代；默认 https://api.telegram.org
	APIBase string `json:"api_base,omitempty"`
}

// TelegramChannel 通过 Bot API 发送告警。
type TelegramChannel struct {
	name   string
	cfg    TelegramConfig
	client *http.Client
}

// NewTelegramChannel 创建 Telegram 渠道。
func NewTelegramChannel(name string, cfg TelegramConfig) *TelegramChannel {
	base := cfg.APIBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	cfg.APIBase = base
	return &TelegramChannel{
		name: name, cfg: cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Name 返回渠道名称。
func (t *TelegramChannel) Name() string { return t.name }

// telegramParseMode 归一化 ParseMode，返回 Telegram 认可的字面量。
//
// 只有 HTML / MarkdownV2 有对应的转义器，因此也只有这两种才允许声明 parse_mode：
// 声明一个自己没有转义过的模式，等于把不可信字符串直接交给 Telegram 的解析器。
// 未识别的值（含 "Markdown" V1）一律退回纯文本。
func telegramParseMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "html":
		return "HTML"
	case "markdownv2":
		return "MarkdownV2"
	}
	return ""
}

// Send 发送消息。按 ParseMode 选择转义方式，并把该模式一并声明给 Telegram（13.4.4）。
func (t *TelegramChannel) Send(ctx context.Context, m Message) error {
	mode := telegramParseMode(t.cfg.ParseMode)
	var text string
	switch mode {
	case "MarkdownV2":
		text = "*" + EscapeTelegramMarkdownV2(m.Title) + "*\n" + EscapeTelegramMarkdownV2(m.Body)
	case "HTML":
		text = "<b>" + EscapeTelegramHTML(m.Title) + "</b>\n" + EscapeTelegramHTML(m.Body)
	default:
		text = m.Title + "\n" + m.Body
	}

	payload := map[string]any{
		"chat_id": t.cfg.ChatID,
		"text":    text,
	}
	// 关键：parse_mode 必须与转义方式同时生效。
	//
	// 这里曾漏掉过这一项——代码按 HTML/MarkdownV2 转义了文本、把标题包成
	// `<b>…</b>` 或 `*…*`，却没告诉 Telegram 用哪种解析模式。Telegram 于是按
	// 纯文本渲染，用户收到的是字面上的 `&lt;b&gt;[WARN] node#1&lt;/b&gt;`，
	// 而 API 返回 200、日志一切正常，只有真盯着 Telegram 才看得出。
	if mode != "" {
		payload["parse_mode"] = mode
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", t.cfg.APIBase, t.cfg.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// ---------------------------------------------------------------------------
// SMTP
// ---------------------------------------------------------------------------

// SMTPConfig 是 alert_channels.config_enc 解密后的内容。
type SMTPConfig struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	// StartTLS 为 true 时使用 STARTTLS（默认 true）。
	StartTLS *bool `json:"start_tls,omitempty"`
}

// SMTPChannel 通过 SMTP 发送告警邮件。
type SMTPChannel struct {
	name string
	cfg  SMTPConfig
}

// NewSMTPChannel 创建 SMTP 渠道。
func NewSMTPChannel(name string, cfg SMTPConfig) *SMTPChannel {
	return &SMTPChannel{name: name, cfg: cfg}
}

// Name 返回渠道名称。
func (s *SMTPChannel) Name() string { return s.name }

// Send 发送邮件。主题与头部移除 CR/LF（13.4.4）。
func (s *SMTPChannel) Send(ctx context.Context, m Message) error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)

	subject := SanitizeHeader(m.Title)
	from := SanitizeHeader(s.cfg.From)
	to := make([]string, 0, len(s.cfg.To))
	for _, a := range s.cfg.To {
		to = append(to, SanitizeHeader(a))
	}

	var body bytes.Buffer
	fmt.Fprintf(&body, "From: %s\r\n", from)
	fmt.Fprintf(&body, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&body, "Subject: %s\r\n", subject)
	body.WriteString("MIME-Version: 1.0\r\n")
	body.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	body.WriteString("\r\n")
	body.WriteString(stripControl(m.Body))

	startTLS := true
	if s.cfg.StartTLS != nil {
		startTLS = *s.cfg.StartTLS
	}

	// 连接。startTLS=true 时使用明文连接 + STARTTLS；false 时若端口为 465 用隐式 TLS。
	conn, err := dialSMTP(ctx, addr, s.cfg.Host, s.cfg.Port, startTLS)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return err
	}
	defer func() { _ = c.Quit() }()

	if startTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
				return err
			}
		}
	}
	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, a := range to {
		if err := c.Rcpt(a); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body.Bytes()); err != nil {
		return err
	}
	return w.Close()
}

// dialSMTP 建立 SMTP 连接。
// startTLS=true → 明文 TCP（随后 STARTTLS）；false 且端口为 465 → 隐式 TLS。
func dialSMTP(ctx context.Context, addr, host string, port int, startTLS bool) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if !startTLS && port == 465 {
		return tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host})
	}
	return d.DialContext(ctx, "tcp", addr)
}
