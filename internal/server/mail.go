package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// 邮件投递是后台任务：固定数量 worker + 有界队列，不用每请求一个 goroutine，
// 也不允许无界排队。队列满时入队立即失败（调用方自行决定是否暴露给用户）。
const (
	mailWorkerCount   = 2
	mailQueueCapacity = 64
	mailDialTimeout   = 10 * time.Second
	mailIOTimeout     = 20 * time.Second
)

// mailMessage 是一封纯文本邮件。
type mailMessage struct {
	To      string
	Subject string
	Text    string
}

// mailSender 是实际投递实现。App.mailSend 默认是 sendSMTPMail；
// 测试可以在创建 App 后替换成注入实现，不需要真实 SMTP 服务。
type mailSender func(ctx context.Context, msg mailMessage) error

// mailJob 是一次账号邮件投递任务（目前只有找回密码走队列）。
// 令牌在 worker 内部生成，只存在于内存与发信内容里，数据库只保存哈希。
type mailJob struct {
	to      string
	purpose string
}

var errMailUnavailable = errors.New("邮件功能未配置")

// mailAvailable 表示当前部署是否具备发送账号邮件的能力：
// 需要完整的 SMTP 配置，并且 BASEURL 可用于生成绝对链接（链接域名不接受请求头或用户输入）。
func (a *App) mailAvailable() bool {
	return a.cfg.SMTP.Configured() && a.cfg.BaseURL != ""
}

// enqueueRecoveryMail 尝试把投递任务放进有界队列，不阻塞、不起新 goroutine。
// 返回 false 表示队列已满或应用正在关闭。
func (a *App) enqueueRecoveryMail(job mailJob) bool {
	if a.mailJobs == nil {
		return false
	}
	select {
	case a.mailJobs <- job:
		return true
	default:
		return false
	}
}

// startMailWorkers 启动固定数量的投递 worker；它们随 backgroundCancel 结束。
func (a *App) startMailWorkers(ctx context.Context) {
	if a.mailJobs == nil {
		return
	}
	for i := 0; i < mailWorkerCount; i++ {
		a.backgroundWG.Add(1)
		go func() {
			defer a.backgroundWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-a.mailJobs:
					a.deliverRecoveryMail(ctx, job)
				}
			}
		}()
	}
}

// deliverRecoveryMail 处理一封找回密码邮件：先按邮箱查账号（不存在/已禁用就默默结束，
// 不向调用方或日志透露），再签发令牌并投递。错误只写日志：找回密码不能通过投递结果
// 泄露账号是否存在，日志里也不写收件人、令牌或链接。
func (a *App) deliverRecoveryMail(ctx context.Context, job mailJob) {
	record, err := a.loadUserByEmail(ctx, job.to)
	if errors.Is(err, errNoUser) {
		return
	}
	if err != nil {
		writeMailLog("读取找回密码账号失败", err)
		return
	}
	if record.disabled {
		return
	}
	if err := a.issueAccountToken(ctx, record.user.ID, record.user.Email, record.user.SessionVersion, job.to, job.purpose); err != nil {
		writeMailLog("投递账号邮件失败", err)
	}
}

// recoveryMailMessage 根据配置与任务生成邮件内容；纯函数，便于测试链接与文案。
func (a *App) recoveryMailMessage(purpose, to, token string) (mailMessage, error) {
	if a.cfg.SMTP.From == "" {
		return mailMessage{}, errMailUnavailable
	}
	if a.cfg.BaseURL == "" {
		return mailMessage{}, errMailUnavailable
	}
	path := "/reset-password"
	subject := "PicBox 密码重置"
	intro := "你申请了重置 PicBox 密码。"
	switch purpose {
	case accountTokenPurposeEmailChange:
		path = "/verify-email"
		subject = "PicBox 确认修改登录邮箱"
		intro = "你申请将 PicBox 登录邮箱修改为此地址。"
	case accountTokenPurposePasswordReset:
	default:
		return mailMessage{}, fmt.Errorf("未知的账号令牌用途: %q", purpose)
	}
	// 原版把令牌放在 URL fragment 里（#token=...），因此不会进入服务器访问日志，
	// 页面读取后立即用 replaceState 清除。
	link := a.cfg.BaseURL + path + "#token=" + token
	text := intro + "\n\n请打开以下链接完成操作：\n" + link +
		"\n\n链接 30 分钟内有效，只能使用一次；重新申请后旧链接失效。\n" +
		"如果不是你本人发起的操作，请忽略此邮件。"
	return mailMessage{To: to, Subject: subject, Text: text}, nil
}

// sendSMTPMail 通过 SMTP 投递一封纯文本邮件。
//
// TLS 语义：
//   - SMTP_SECURE=true：连接建立即 TLS（隐式 TLS，通常 465）；
//   - 否则默认要求 STARTTLS（SMTP_REQUIRE_TLS 默认 true），服务器不支持则失败；
//   - 只有部署显式关闭两者时才允许明文，且不会尝试用明文发送密码（net/smtp 的
//     PlainAuth 在未加密连接上直接拒绝）。
//
// 证书校验始终启用：没有 InsecureSkipVerify 开关。
func (a *App) sendSMTPMail(ctx context.Context, msg mailMessage) error {
	cfg := a.cfg.SMTP
	if !cfg.Configured() {
		return errMailUnavailable
	}
	address, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return fmt.Errorf("SMTP_FROM 非法: %w", err)
	}
	recipient, err := mail.ParseAddress(msg.To)
	if err != nil {
		return fmt.Errorf("收件地址非法: %w", err)
	}

	tlsConfig := &tls.Config{
		ServerName: cfg.Host,
		MinVersion: tls.VersionTLS12,
	}

	dialer := &net.Dialer{Timeout: mailDialTimeout}
	var conn net.Conn
	if cfg.Secure {
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsConfig}
		conn, err = tlsDialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port)))
		if err != nil {
			return fmt.Errorf("连接 SMTP 失败: %w", err)
		}
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port)))
		if err != nil {
			return fmt.Errorf("连接 SMTP 失败: %w", err)
		}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(mailIOTimeout))

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	defer func() { _ = client.Close() }()

	if !cfg.Secure {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsConfig); err != nil {
				return fmt.Errorf("SMTP STARTTLS 失败: %w", err)
			}
		} else if cfg.RequireTLS {
			// 明确失败，绝不静默降级为明文投递。
			return errors.New("SMTP 服务器不支持 STARTTLS，且配置要求 TLS")
		}
	}

	if cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err := client.Mail(address.Address); err != nil {
		return fmt.Errorf("SMTP MAIL FROM 失败: %w", err)
	}
	if err := client.Rcpt(recipient.Address); err != nil {
		return fmt.Errorf("SMTP RCPT TO 失败: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA 失败: %w", err)
	}
	if _, err := writer.Write(composePlainTextMail(address.String(), msg)); err != nil {
		_ = writer.Close()
		return fmt.Errorf("写入 SMTP 内容失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("结束 SMTP DATA 失败: %w", err)
	}
	if err := client.Quit(); err != nil {
		// 邮件已被服务器接受；Quit 失败不改变投递结果。
		return nil
	}
	return nil
}

// composePlainTextMail 生成 UTF-8 纯文本邮件（正文 base64，主题按 RFC 2047 编码）。
func composePlainTextMail(from string, msg mailMessage) []byte {
	var builder strings.Builder
	builder.WriteString("From: " + from + "\r\n")
	builder.WriteString("To: " + msg.To + "\r\n")
	builder.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", msg.Subject) + "\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")
	builder.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	builder.WriteString("Content-Transfer-Encoding: base64\r\n")
	builder.WriteString("\r\n")

	encoded := base64.StdEncoding.EncodeToString([]byte(msg.Text))
	for len(encoded) > 76 {
		builder.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	if encoded != "" {
		builder.WriteString(encoded + "\r\n")
	}
	return []byte(builder.String())
}

// newAccountToken 生成 32 字节随机令牌（64 位小写 hex），返回令牌与入库哈希。
func newAccountToken() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("生成账号令牌失败: %w", err)
	}
	token := hex.EncodeToString(raw)
	return token, hashAccountTokenHex(token), nil
}

// hashAccountTokenHex 计算令牌的 sha256 哈希；调用方必须先用 accountTokenPattern 校验格式。
func hashAccountTokenHex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writeMailLog(message string, err error) {
	// 只记录错误类别，不记录收件人、令牌或链接。
	log.Printf("mail: %s: %v", message, err)
}
