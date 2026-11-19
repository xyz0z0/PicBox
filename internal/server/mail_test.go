package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fake SMTP server（本地 TCP，不访问外网） ----

type coreFakeSMTP struct {
	listener   net.Listener
	advertise  bool // 是否在 EHLO 里声明 STARTTLS
	mu         sync.Mutex
	mailFrom   []string
	recipients []string
	data       []string
}

func coreStartFakeSMTP(t *testing.T, advertiseStartTLS bool) *coreFakeSMTP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动假 SMTP 失败: %v", err)
	}
	server := &coreFakeSMTP{listener: listener, advertise: advertiseStartTLS}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func (s *coreFakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	address, ok := s.listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("假 SMTP 地址类型异常: %T", s.listener.Addr())
	}
	return "127.0.0.1", address.Port
}

func (s *coreFakeSMTP) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	write := func(line string) {
		_, _ = writer.WriteString(line + "\r\n")
		_ = writer.Flush()
	}

	write("220 fake ESMTP")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.TrimSpace(line)
		upper := strings.ToUpper(command)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if s.advertise {
				write("250-fake")
				write("250 STARTTLS")
			} else {
				write("250-fake")
				write("250 8BITMIME")
			}
		case strings.HasPrefix(upper, "HELO"):
			write("250 fake")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.mu.Lock()
			s.mailFrom = append(s.mailFrom, command)
			s.mu.Unlock()
			write("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			s.mu.Lock()
			s.recipients = append(s.recipients, command)
			s.mu.Unlock()
			write("250 OK")
		case upper == "DATA":
			write("354 End data with <CR><LF>.<CR><LF>")
			var body strings.Builder
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dataLine, "\r\n") == "." {
					break
				}
				body.WriteString(dataLine)
			}
			s.mu.Lock()
			s.data = append(s.data, body.String())
			s.mu.Unlock()
			write("250 OK queued")
		case upper == "QUIT":
			write("221 bye")
			return
		default:
			write("250 OK")
		}
	}
}

func (s *coreFakeSMTP) snapshot() (mailFrom, recipients, data []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.mailFrom...), append([]string(nil), s.recipients...), append([]string(nil), s.data...)
}

func TestMailAvailableRequiresSMTPAndBaseURL(t *testing.T) {
	app := coreTestApp(t, Config{})
	if app.mailAvailable() {
		t.Fatalf("未配置 SMTP 时邮件能力必须不可用")
	}

	appWithSMTP := coreTestApp(t, Config{SMTP: SMTPConfig{Host: "smtp.example.com", From: "a@example.com"}})
	if appWithSMTP.mailAvailable() {
		t.Fatalf("缺少 BASEURL 时无法生成链接，邮件能力必须不可用")
	}

	appReady := coreTestApp(t, Config{
		BaseURL: "https://pic.example.com",
		SMTP:    SMTPConfig{Host: "smtp.example.com", From: "a@example.com"},
	})
	if !appReady.mailAvailable() {
		t.Fatalf("SMTP 与 BASEURL 都配置时应可用")
	}
}

func TestRecoveryMailMessageLinksAndPurpose(t *testing.T) {
	app := coreTestApp(t, Config{
		BaseURL: "https://pic.example.com",
		SMTP:    SMTPConfig{Host: "smtp.example.com", From: "PicBox <noreply@example.com>"},
	})

	reset, err := app.recoveryMailMessage(accountTokenPurposePasswordReset, "user@example.com", "abc123")
	if err != nil {
		t.Fatalf("构造找回密码邮件失败: %v", err)
	}
	if reset.To != "user@example.com" || !strings.Contains(reset.Subject, "密码重置") {
		t.Fatalf("找回密码邮件字段不正确: %+v", reset)
	}
	if !strings.Contains(reset.Text, "https://pic.example.com/reset-password#token=abc123") {
		t.Fatalf("找回密码链接不正确: %s", reset.Text)
	}
	if strings.Contains(reset.Text, "?token=") {
		t.Fatalf("令牌必须放在 fragment 而不是查询串里: %s", reset.Text)
	}

	change, err := app.recoveryMailMessage(accountTokenPurposeEmailChange, "new@example.com", "def456")
	if err != nil {
		t.Fatalf("构造改邮箱邮件失败: %v", err)
	}
	if !strings.Contains(change.Subject, "邮箱") || !strings.Contains(change.Text, "/verify-email#token=def456") {
		t.Fatalf("改邮箱邮件内容不正确: %+v", change)
	}

	if _, err := app.recoveryMailMessage("UNKNOWN", "user@example.com", "x"); err == nil {
		t.Fatalf("未知用途必须报错")
	}
	unconfigured := coreTestApp(t, Config{BaseURL: "https://pic.example.com"})
	if _, err := unconfigured.recoveryMailMessage(accountTokenPurposePasswordReset, "user@example.com", "x"); err == nil {
		t.Fatalf("未配置 SMTP_FROM 时必须报错")
	}
}

func TestComposePlainTextMailEncodesUtf8(t *testing.T) {
	body := "第一行\n第二行 " + strings.Repeat("内容", 80)
	raw := composePlainTextMail("PicBox <noreply@example.com>", mailMessage{
		To:      "user@example.com",
		Subject: "PicBox 密码重置",
		Text:    body,
	})
	text := string(raw)
	if !strings.Contains(text, "Content-Type: text/plain; charset=utf-8") {
		t.Fatalf("缺少纯文本 MIME 头: %s", text)
	}
	if !strings.Contains(text, "Subject: =?utf-8?") {
		t.Fatalf("非 ASCII 主题必须按 RFC 2047 编码: %s", text)
	}

	_, encodedBody, found := strings.Cut(text, "\r\n\r\n")
	if !found {
		t.Fatalf("缺少头/体分隔: %s", text)
	}
	for _, line := range strings.Split(strings.TrimRight(encodedBody, "\r\n"), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("base64 行不得超过 76 字符: %d", len(line))
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimRight(encodedBody, "\r\n"), "\r\n", ""))
	if err != nil {
		t.Fatalf("正文不是合法 base64: %v", err)
	}
	if string(decoded) != body {
		t.Fatalf("正文解码后应与原文一致")
	}
}

func TestSendSMTPMailDeliversPlainTextOverExplicitPlaintext(t *testing.T) {
	server := coreStartFakeSMTP(t, false)
	host, port := server.hostPort(t)
	app := coreTestApp(t, Config{
		BaseURL: "https://pic.example.com",
		// 显式关闭 secure 与 requireTLS：部署明确选择明文，服务不会静默降级。
		SMTP: SMTPConfig{Host: host, Port: port, From: "noreply@example.com"}.AllowExplicitPlaintext(),
	})

	if err := app.sendSMTPMail(context.Background(), mailMessage{
		To:      "user@example.com",
		Subject: "PicBox 测试",
		Text:    "hello",
	}); err != nil {
		t.Fatalf("显式明文投递应当成功: %v", err)
	}

	mailFrom, recipients, data := server.snapshot()
	if len(mailFrom) != 1 || !strings.Contains(mailFrom[0], "noreply@example.com") {
		t.Fatalf("MAIL FROM 不正确: %v", mailFrom)
	}
	if len(recipients) != 1 || !strings.Contains(recipients[0], "user@example.com") {
		t.Fatalf("RCPT TO 不正确: %v", recipients)
	}
	if len(data) != 1 || !strings.Contains(data[0], "Subject:") {
		t.Fatalf("DATA 内容不正确: %v", data)
	}
}

func TestSendSMTPMailNeverDowngradesWhenStartTLSRequired(t *testing.T) {
	server := coreStartFakeSMTP(t, false)
	host, port := server.hostPort(t)
	app := coreTestApp(t, Config{
		SMTP: SMTPConfig{Host: host, Port: port, RequireTLS: true, From: "noreply@example.com"},
	})

	err := app.sendSMTPMail(context.Background(), mailMessage{To: "user@example.com", Subject: "s", Text: "t"})
	if err == nil {
		t.Fatalf("服务器不支持 STARTTLS 时必须失败")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("错误应说明 STARTTLS 缺失: %v", err)
	}
	if _, recipients, data := server.snapshot(); len(recipients) != 0 || len(data) != 0 {
		t.Fatalf("绝不能在没有 TLS 的连接上投递: recipients=%v data=%v", recipients, data)
	}
}

func TestSendSMTPMailReportsConnectionFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败: %v", err)
	}
	address := listener.Addr().(*net.TCPAddr)
	_ = listener.Close() // 端口已释放：连接必然失败

	app := coreTestApp(t, Config{
		SMTP: SMTPConfig{Host: "127.0.0.1", Port: address.Port, From: "noreply@example.com"}.AllowExplicitPlaintext(),
	})
	deliverErr := app.sendSMTPMail(context.Background(), mailMessage{To: "user@example.com", Subject: "s", Text: "t"})
	if deliverErr == nil {
		t.Fatalf("连接失败必须返回错误")
	}
	if !strings.Contains(deliverErr.Error(), "SMTP") {
		t.Fatalf("错误信息应标明 SMTP 阶段: %v", deliverErr)
	}
}

func TestMailQueueIsBounded(t *testing.T) {
	app := &App{mailJobs: make(chan mailJob, 1)}
	if !app.enqueueRecoveryMail(mailJob{to: "a@example.com", purpose: accountTokenPurposePasswordReset}) {
		t.Fatalf("队列未满时应入队成功")
	}
	if app.enqueueRecoveryMail(mailJob{to: "b@example.com", purpose: accountTokenPurposePasswordReset}) {
		t.Fatalf("队列满时必须立即失败，不能阻塞或无限堆积")
	}

	empty := &App{}
	if empty.enqueueRecoveryMail(mailJob{to: "a@example.com"}) {
		t.Fatalf("未初始化队列时不得入队")
	}
}

func TestCloseCancelsInFlightMailDelivery(t *testing.T) {
	app, capture := coreMailApp(t)
	coreRegister(t, app, "closing@example.com", "secret123")

	blocking := make(chan struct{})
	capture.mu.Lock()
	capture.err = nil
	capture.mu.Unlock()
	app.mailSend = func(ctx context.Context, _ mailMessage) error {
		close(blocking)
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled")
		case <-time.After(10 * time.Second):
			return nil
		}
	}

	response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/forgot-password",
		map[string]string{"email": "closing@example.com"}, nil)
	if response.status != http.StatusOK {
		t.Fatalf("找回密码应当 200: %d %v", response.status, response.body)
	}

	select {
	case <-blocking:
	case <-time.After(3 * time.Second):
		t.Fatalf("后台投递未开始")
	}

	done := make(chan error, 1)
	go func() { done <- app.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close 失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Close 必须在取消后台任务后返回")
	}
}
