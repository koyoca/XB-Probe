package alert

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 文本清洗与转义
// ---------------------------------------------------------------------------

func TestBuildMessageEscapesControlChars(t *testing.T) {
	cases := []struct {
		name  string
		level string
		node  string
		kind  string
		detl  string
		want  string
	}{
		{
			name:  "常规",
			level: "warn", node: "东京", kind: "cpu", detl: "90.0% ≥ 90.0%",
			want: "WARN | 东京 | cpu | 90.0% ≥ 90.0%",
		},
		{
			name:  "级别转大写",
			level: "error", node: "n", kind: "offline", detl: "x",
			want: "ERROR | n | offline | x",
		},
		{
			// 节点名来自 Agent 上报，是不可信输入：换行必须被压平，
			// 否则一条告警能在日志/消息里伪造出多行。
			name: "换行压平", level: "info", node: "a\nb", kind: "ok", detl: "1\r\n2",
			want: "INFO | a b | ok | 1  2",
		},
		{
			name: "控制字符删除", level: "info", node: "a\x00\x01b", kind: "k\x7f", detl: "d\x1b[31m",
			want: "INFO | ab | k | d[31m",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildMessage(tc.level, tc.node, tc.kind, tc.detl); got != tc.want {
				t.Errorf("BuildMessage = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestStripControlKeepsNormalWhitespace(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"中文和 English", "中文和 English"},
		{"a b", "a b"},       // 普通空格保留
		{"a\tb", "a b"},      // 制表符 → 空格
		{"a\nb", "a b"},      // 换行 → 空格
		{"a\r\nb", "a  b"},   // CR 与 LF 各占一位
		{"a\x00b", "ab"},     // NUL 删除
		{"a\x7fb", "ab"},     // DEL 删除
		{"a\x1bbc", "abc"},   // ESC 删除
		{"\u200b", "\u200b"}, // 零宽空格不是控制字符，保留原样
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripControl(tc.in); got != tc.want {
			t.Errorf("stripControl(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestSanitizeHeaderNeutralisesInjection 邮件头注入防护。
//
// From / To / Subject 都来自后台配置，而"节点名"这类值会进 Subject。
// 一旦 CR/LF 能穿过去，一条告警就能给对方邮件系统塞进 Bcc: ——这里必须断言
// 结果里**一个换行都没有**，而不是只断言"看起来正常"。
func TestSanitizeHeaderNeutralisesInjection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"常规", "Server Probe 告警", "Server Probe 告警"},
		{"CRLF 折叠", "OK\r\nBcc: attacker@evil.com", "OK Bcc: attacker@evil.com"},
		{"裸 LF", "OK\nX-Injected: 1", "OK X-Injected: 1"},
		{"裸 CR", "OK\rX-Injected: 1", "OKX-Injected: 1"},
		{"首尾空白", "  spaced  ", "spaced"},
		{"多行折叠", "a\r\nb\nc", "a b c"},
		{"空", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeHeader(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeHeader(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Fatalf("结果里仍存在换行，可被用于头注入: %q", got)
			}
		})
	}
}

func TestEscapeTelegramHTML(t *testing.T) {
	// 期望值按 Go html.EscapeString 的映射硬编码：& ' < > " 五个字符。
	got := EscapeTelegramHTML(`<b>a & b</b> "x" 'y'`)
	want := `&lt;b&gt;a &amp; b&lt;/b&gt; &#34;x&#34; &#39;y&#39;`
	if got != want {
		t.Errorf("EscapeTelegramHTML = %q，期望 %q", got, want)
	}
	// 控制字符先被清洗再转义。
	if got := EscapeTelegramHTML("a\nb"); got != "a b" {
		t.Errorf("换行应先被清洗，得到 %q", got)
	}
	if got := EscapeTelegramHTML("a\x00<b"); got != "a&lt;b" {
		t.Errorf("NUL 应被清洗，得到 %q", got)
	}
}

// TestEscapeTelegramMarkdownV2EscapesEveryReservedChar 逐个字符验。
//
// 只断言"某几个字符被转义"是不够的：MarkdownV2 的保留字符表一旦漏掉一个
// （比如 `-` 或 `{`），含该字符的节点名会让整条消息发送失败（Telegram 返回
// 400 can't parse entities），而本地演示里根本发现不了。
func TestEscapeTelegramMarkdownV2EscapesEveryReservedChar(t *testing.T) {
	const reserved = `_*[]()~` + "`" + `>#+-=|{}.!\`

	for _, r := range reserved {
		got := EscapeTelegramMarkdownV2(string(r))
		want := "\\" + string(r)
		if got != want {
			t.Errorf("保留字符 %q 应转义为 %q，得到 %q", string(r), want, got)
		}
	}

	// 组合场景：告警标题里的方括号、井号、圆括号都要带反斜杠。
	if got, want := EscapeTelegramMarkdownV2("[WARN] node#1 (cpu)"),
		`\[WARN\] node\#1 \(cpu\)`; got != want {
		t.Errorf("组合转义 = %q，期望 %q", got, want)
	}
	// 反斜杠自身也要转义，否则会吃掉后面那个字符的转义。
	if got, want := EscapeTelegramMarkdownV2(`a\b`), `a\\b`; got != want {
		t.Errorf("反斜杠转义 = %q，期望 %q", got, want)
	}
	// 非保留字符（含中文）保持原样。
	if got := EscapeTelegramMarkdownV2("纯文本 abc123 中文"); got != "纯文本 abc123 中文" {
		t.Errorf("非保留字符不应被改动，得到 %q", got)
	}
}

// ---------------------------------------------------------------------------
// Telegram
// ---------------------------------------------------------------------------

// telegramRecorder 起一个假的 Bot API，记录请求路径与 JSON 载荷。
type telegramRecorder struct {
	*httptest.Server
	mu      sync.Mutex
	path    string
	payload map[string]any
	calls   int
}

func newTelegramRecorder(t *testing.T, status int, body string) *telegramRecorder {
	t.Helper()
	rec := &telegramRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.calls++
		rec.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&rec.payload)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (r *telegramRecorder) snapshot() (string, map[string]any, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path, r.payload, r.calls
}

const testBotToken = "123456:AAHfakeTOKEN"

func TestNewTelegramChannelDefaultsAPIBase(t *testing.T) {
	ch := NewTelegramChannel("tg", TelegramConfig{BotToken: testBotToken})
	if ch.cfg.APIBase != "https://api.telegram.org" {
		t.Errorf("APIBase 应默认为官方地址，得到 %q", ch.cfg.APIBase)
	}
	if ch.Name() != "tg" {
		t.Errorf("Name() = %q，期望 tg", ch.Name())
	}
	if ch.client == nil || ch.client.Timeout != 10*time.Second {
		t.Error("HTTP 客户端应有 10 秒超时，避免告警链路永久挂住")
	}
}

func TestTelegramSendPlainTextByDefault(t *testing.T) {
	rec := newTelegramRecorder(t, 200, `{"ok":true}`)
	ch := NewTelegramChannel("tg", TelegramConfig{
		BotToken: testBotToken, ChatID: "-100123", APIBase: rec.URL,
	})

	title, body := "Server Probe 测试消息", "渠道配置正常。"
	if err := ch.Send(context.Background(), Message{Title: title, Body: body}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	path, payload, calls := rec.snapshot()
	if calls != 1 {
		t.Fatalf("应只请求 1 次，实际 %d 次", calls)
	}
	if want := "/bot" + testBotToken + "/sendMessage"; path != want {
		t.Errorf("请求路径 = %q，期望 %q", path, want)
	}
	if payload["chat_id"] != "-100123" {
		t.Errorf("chat_id = %v，期望 -100123", payload["chat_id"])
	}
	if got, want := payload["text"], title+"\n"+body; got != want {
		t.Errorf("text = %q，期望 %q", got, want)
	}
	// 未指定解析模式时不能瞎声明 parse_mode，否则 Telegram 会按它解析纯文本。
	if _, ok := payload["parse_mode"]; ok {
		t.Errorf("未配置解析模式时不应发送 parse_mode，得到 %v", payload["parse_mode"])
	}
}

// TestTelegramSendDeclaresParseModeItEscapedFor 转义与 parse_mode 必须同时生效。
//
// 这是本条链路上最隐蔽的坑：代码按 parse_mode 选择了转义方式、把标题包成
// `<b>…</b>` 或 `*…*`，却**没有把这个 parse_mode 发给 Telegram**。
// 于是 Telegram 按纯文本渲染，用户收到的是字面上的尖括号和反斜杠：
// `&lt;b&gt;[OK] node#1&lt;/b&gt;` 之类，看起来像乱码，而服务端返回 200、
// 日志一切正常——只有真的盯着 Telegram 看才会发现。
func TestTelegramSendDeclaresParseModeItEscapedFor(t *testing.T) {
	title := "<b>hi</b> & co"
	body := "a<b"

	t.Run("HTML 大小写不敏感", func(t *testing.T) {
		rec := newTelegramRecorder(t, 200, `{"ok":true}`)
		// 故意写小写，验证归一化到 Telegram 认可的大小写。
		ch := NewTelegramChannel("tg", TelegramConfig{
			BotToken: testBotToken, ChatID: "1", ParseMode: "html", APIBase: rec.URL,
		})
		if err := ch.Send(context.Background(), Message{Title: title, Body: body}); err != nil {
			t.Fatalf("发送失败: %v", err)
		}
		_, payload, _ := rec.snapshot()
		if got := payload["parse_mode"]; got != "HTML" {
			t.Errorf("parse_mode = %v，期望 HTML（必须随转义一起声明）", got)
		}
		want := "<b>" + EscapeTelegramHTML(title) + "</b>\n" + EscapeTelegramHTML(body)
		if got := payload["text"]; got != want {
			t.Errorf("text = %q，期望 %q", got, want)
		}
		// 转义后的文本里不能出现未转义的原始尖括号。
		if text, _ := payload["text"].(string); strings.Contains(text, "<b>hi</b>") {
			t.Error("HTML 模式下不可信字符串必须被转义，不能原样发出")
		}
	})

	t.Run("MarkdownV2", func(t *testing.T) {
		rec := newTelegramRecorder(t, 200, `{"ok":true}`)
		ch := NewTelegramChannel("tg", TelegramConfig{
			BotToken: testBotToken, ChatID: "1", ParseMode: "MarkdownV2", APIBase: rec.URL,
		})
		mdTitle, mdBody := "[WARN] node#1 (cpu)", "90.0% ≥ 90.0% - 5m"
		if err := ch.Send(context.Background(), Message{Title: mdTitle, Body: mdBody}); err != nil {
			t.Fatalf("发送失败: %v", err)
		}
		_, payload, _ := rec.snapshot()
		if got := payload["parse_mode"]; got != "MarkdownV2" {
			t.Errorf("parse_mode = %v，期望 MarkdownV2", got)
		}
		want := "*" + EscapeTelegramMarkdownV2(mdTitle) + "*\n" + EscapeTelegramMarkdownV2(mdBody)
		if got := payload["text"]; got != want {
			t.Errorf("text = %q，期望 %q", got, want)
		}
	})

	t.Run("未知模式退回纯文本且不声明", func(t *testing.T) {
		// "Markdown"（V1）本实现没有专门的转义器，必须退回纯文本，
		// 而不是声明一个自己没转义过的模式。
		rec := newTelegramRecorder(t, 200, `{"ok":true}`)
		ch := NewTelegramChannel("tg", TelegramConfig{
			BotToken: testBotToken, ChatID: "1", ParseMode: "Markdown", APIBase: rec.URL,
		})
		if err := ch.Send(context.Background(), Message{Title: "t", Body: "b"}); err != nil {
			t.Fatalf("发送失败: %v", err)
		}
		_, payload, _ := rec.snapshot()
		if _, ok := payload["parse_mode"]; ok {
			t.Errorf("未识别的解析模式不应声明，得到 %v", payload["parse_mode"])
		}
		if got := payload["text"]; got != "t\nb" {
			t.Errorf("text = %q，期望原样纯文本", got)
		}
	})
}

func TestTelegramSendSurfacesHTTPError(t *testing.T) {
	rec := newTelegramRecorder(t, 400, "Bad Request: chat not found")
	ch := NewTelegramChannel("tg", TelegramConfig{
		BotToken: testBotToken, ChatID: "x", APIBase: rec.URL,
	})

	err := ch.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("HTTP 400 应返回错误")
	}
	// 错误信息要带上响应体，否则运维看不出是 chat_id 错了还是 token 过期。
	if !strings.Contains(err.Error(), "telegram: HTTP 400") {
		t.Errorf("错误信息应含状态码，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("错误信息应含响应体，得到 %v", err)
	}
	// 上游不同步返回 4xx 时也不该无限读响应体。
	if len(err.Error()) > 1024 {
		t.Errorf("错误信息过长（应被 LimitReader 截断）: %d 字节", len(err.Error()))
	}
}

func TestTelegramSendFailsOnCanceledContext(t *testing.T) {
	rec := newTelegramRecorder(t, 200, `{"ok":true}`)
	ch := NewTelegramChannel("tg", TelegramConfig{
		BotToken: testBotToken, ChatID: "x", APIBase: rec.URL,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := ch.Send(ctx, Message{Title: "t"}); err == nil {
		t.Fatal("ctx 已取消时发送应失败")
	}
}

func TestTelegramSendTruncatesLongErrorBody(t *testing.T) {
	rec := newTelegramRecorder(t, 500, strings.Repeat("x", 4096))
	ch := NewTelegramChannel("tg", TelegramConfig{
		BotToken: testBotToken, ChatID: "x", APIBase: rec.URL,
	})

	err := ch.Send(context.Background(), Message{Title: "t"})
	if err == nil {
		t.Fatal("HTTP 500 应返回错误")
	}
	if strings.Count(err.Error(), "x") > 512 {
		t.Errorf("响应体应被截断到 512 字节，实际 %d 个 x", strings.Count(err.Error(), "x"))
	}
}

// ---------------------------------------------------------------------------
// SMTP
// ---------------------------------------------------------------------------

// fakeSMTPServer 是一个极简 SMTP 服务端：只走 EHLO/MAIL/RCPT/DATA/QUIT，
// **不**通告 STARTTLS 与 AUTH。够用来验证客户端真正写出的报文内容。
type fakeSMTPServer struct {
	ln   net.Listener
	mu   sync.Mutex
	data []string
}

func startFakeSMTPServer(t *testing.T) (*fakeSMTPServer, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	s := &fakeSMTPServer{ln: ln}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s, ln.Addr().String()
}

func (s *fakeSMTPServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *fakeSMTPServer) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	reply := func(line string) { _, _ = c.Write([]byte(line + "\r\n")) }

	reply("220 fake ESMTP ready")
	var body strings.Builder
	inData := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.data = append(s.data, body.String())
				s.mu.Unlock()
				body.Reset()
				reply("250 OK: queued")
				continue
			}
			body.WriteString(line)
			body.WriteString("\r\n")
			continue
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			reply("250-fake")
			reply("250 OK")
		case strings.HasPrefix(up, "HELO"):
			reply("250 fake")
		case strings.HasPrefix(up, "MAIL FROM"):
			reply("250 OK")
		case strings.HasPrefix(up, "RCPT TO"):
			reply("250 OK")
		case strings.HasPrefix(up, "DATA"):
			inData = true
			reply("354 End data with <CR><LF>.<CR><LF>")
		case strings.HasPrefix(up, "QUIT"):
			reply("221 Bye")
			return
		default:
			reply("250 OK")
		}
	}
}

func (s *fakeSMTPServer) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.data...)
}

// splitHeaderBody 按第一个空行切开报文。
//
// 正文末尾的 CRLF 属于 DATA 的 `\r\n.\r\n` 终止符（RFC 5321），不属于正文本身，
// 这里剥掉一层，让断言比对"客户端真正写的正文"。
func splitHeaderBody(t *testing.T, raw string) ([]string, string) {
	t.Helper()
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("报文缺少头/体分隔空行:\n%s", raw)
	}
	return strings.Split(parts[0], "\r\n"), strings.TrimSuffix(parts[1], "\r\n")
}

func TestNewSMTPChannelName(t *testing.T) {
	ch := NewSMTPChannel("mail", SMTPConfig{Host: "smtp.example.com", Port: 587})
	if ch.Name() != "mail" {
		t.Errorf("Name() = %q，期望 mail", ch.Name())
	}
}

func TestSMTPChannelWritesExpectedMessage(t *testing.T) {
	srv, addr := startFakeSMTPServer(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	ch := NewSMTPChannel("mail", SMTPConfig{
		Host: host, Port: port,
		From: "probe@example.com",
		To:   []string{"ops@example.com", "oncall@example.com"},
	})
	title, body := "[ERROR] node#1 已离线", "已离线（超过 90 秒无上报）"
	if err := ch.Send(context.Background(), Message{Title: title, Body: body}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	msgs := srv.messages()
	if len(msgs) != 1 {
		t.Fatalf("服务端应收到 1 封邮件，得到 %d 封", len(msgs))
	}
	headers, gotBody := splitHeaderBody(t, msgs[0])

	wantHeaders := []string{
		"From: probe@example.com",
		"To: ops@example.com, oncall@example.com",
		"Subject: " + title,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
	}
	if len(headers) != len(wantHeaders) {
		t.Fatalf("头部行数 = %d，期望 %d：\n%s", len(headers), len(wantHeaders), strings.Join(headers, "\n"))
	}
	for i, want := range wantHeaders {
		if headers[i] != want {
			t.Errorf("第 %d 行头部 = %q，期望 %q", i+1, headers[i], want)
		}
	}
	if gotBody != body {
		t.Errorf("正文 = %q，期望 %q", gotBody, body)
	}
}

// TestSMTPChannelBlocksHeaderInjection 这是 13.4.4 的核心断言：
// 恶意节点名不得在邮件里**新增一个头部**。
//
// 断言方式不能只是"结果里没有 \r\n"——要按头部块逐行检查：
// 一旦 CR/LF 漏过去，`Bcc: x@y` 会自己占一行，收件人列表就被攻击者扩容了。
func TestSMTPChannelBlocksHeaderInjection(t *testing.T) {
	srv, addr := startFakeSMTPServer(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	ch := NewSMTPChannel("mail", SMTPConfig{
		Host: host, Port: port,
		From: "probe@example.com\r\nBcc: injected-from@evil.com",
		To:   []string{"ops@example.com\r\nBcc: injected-to@evil.com"},
	})
	// 节点名会进 Subject，同样是不可信输入。
	evil := "node1\r\nBcc: injected-subject@evil.com"
	if err := ch.Send(context.Background(), Message{Title: evil, Body: "正常正文"}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	msgs := srv.messages()
	if len(msgs) != 1 {
		t.Fatalf("服务端应收到 1 封邮件，得到 %d 封", len(msgs))
	}
	headers, _ := splitHeaderBody(t, msgs[0])

	allowed := []string{"From:", "To:", "Subject:", "MIME-Version:", "Content-Type:"}
	for i, h := range headers {
		ok := false
		for _, p := range allowed {
			if strings.HasPrefix(h, p) {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("第 %d 行不是合法头部（疑似注入）: %q\n完整头部块:\n%s",
				i+1, h, strings.Join(headers, "\n"))
		}
	}
	// 注入内容被折叠进原头部行，不会自己成为一行。
	found := false
	for _, h := range headers {
		if strings.HasPrefix(h, "Subject:") && strings.Contains(h, "injected-subject@evil.com") {
			found = true
		}
	}
	if !found {
		t.Errorf("注入文本应被折叠进 Subject 行，头部块:\n%s", strings.Join(headers, "\n"))
	}
}

// TestSMTPChannelStripsControlCharsFromBody 正文里的控制字符要清洗掉。
func TestSMTPChannelStripsControlCharsFromBody(t *testing.T) {
	srv, addr := startFakeSMTPServer(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	ch := NewSMTPChannel("mail", SMTPConfig{
		Host: host, Port: port, From: "a@example.com", To: []string{"b@example.com"},
	})
	// 末尾带 \x00，若不清洗会原样进报文。
	if err := ch.Send(context.Background(), Message{Title: "t", Body: "正文\x00结束"}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	_, body := splitHeaderBody(t, srv.messages()[0])
	if body != "正文结束" {
		t.Errorf("正文 = %q，期望控制字符被移除", body)
	}
}

func TestSMTPChannelFailsWhenServerUnreachable(t *testing.T) {
	// 占一个端口再关掉，得到一个必然拒绝连接的地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	ch := NewSMTPChannel("mail", SMTPConfig{
		Host: host, Port: port, From: "a@example.com", To: []string{"b@example.com"},
	})
	if err := ch.Send(context.Background(), Message{Title: "t"}); err == nil {
		t.Fatal("连不上服务器时应返回错误")
	}
}

// TestDialSMTPImplicitTLSOnlyWhenPortIs465 校验连接策略分支。
//
// 同一个 addr，用不同的 port 参数（仅用于分支判定）拿到不同结果，
// 从而证明"port=465 且 startTLS=false"确实走的是隐式 TLS 拨号：
// 对着一个明文监听做 TLS 握手必然失败，而明文分支则会成功。
func TestDialSMTPImplicitTLSOnlyWhenPortIs465(t *testing.T) {
	_, addr := startFakeSMTPServer(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 明文分支：能连上。
	conn, err := dialSMTP(ctx, addr, host, port, true)
	if err != nil {
		t.Fatalf("startTLS=true 应走明文拨号并能连上: %v", err)
	}
	_ = conn.Close()

	// 非 465 端口 + startTLS=false → 仍是明文。
	conn, err = dialSMTP(ctx, addr, host, port, false)
	if err != nil {
		t.Fatalf("非 465 端口应走明文拨号: %v", err)
	}
	_ = conn.Close()

	// port=465 + startTLS=false → 隐式 TLS。对端是明文 SMTP，
	// 握手必然失败；若这里返回 nil，说明该分支根本没生效。
	if conn, err := dialSMTP(ctx, addr, host, 465, false); err == nil {
		_ = conn.Close()
		t.Fatal("port=465 且 startTLS=false 应走隐式 TLS，握手应失败")
	}
}

func TestSMTPChannelHonoursExplicitStartTLSFalse(t *testing.T) {
	srv, addr := startFakeSMTPServer(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	off := false
	ch := NewSMTPChannel("mail", SMTPConfig{
		Host: host, Port: port, From: "a@example.com", To: []string{"b@example.com"},
		StartTLS: &off,
	})
	if err := ch.Send(context.Background(), Message{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("显式关闭 STARTTLS 时应能发送: %v", err)
	}
	if len(srv.messages()) != 1 {
		t.Fatalf("应收到 1 封邮件，得到 %d 封", len(srv.messages()))
	}
}

// TestChannelInterfaceSatisfied 防止以后改签名时忘了同步两个实现。
func TestChannelInterfaceSatisfied(t *testing.T) {
	var _ Channel = (*TelegramChannel)(nil)
	var _ Channel = (*SMTPChannel)(nil)
	var _ Channel = (*recChannel)(nil)
}
