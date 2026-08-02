package mailer

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

type Sender interface {
	SendVerification(ctx context.Context, to, code string, expires time.Duration, systemName string) error
}

type SMTP struct {
	host     string
	port     int
	username string
	password string
	from     string
	tlsMode  string
	timeout  time.Duration
}

func NewSMTP(host string, port int, username, password, from, tlsMode string, timeout time.Duration) *SMTP {
	return &SMTP{host: host, port: port, username: username, password: password, from: from, tlsMode: tlsMode, timeout: timeout}
}

func (s *SMTP) SendVerification(ctx context.Context, to, code string, expires time.Duration, systemName string) error {
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("invalid recipient: %w", err)
	}
	from, err := mail.ParseAddress(s.from)
	if err != nil {
		return fmt.Errorf("invalid sender: %w", err)
	}
	minutes := int(expires.Round(time.Minute) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	systemName = strings.TrimSpace(systemName)
	if systemName == "" {
		systemName = "New API"
	}
	subject := mime.QEncoding.Encode("UTF-8", systemName+" QQ 机器人账户绑定验证码")
	body := fmt.Sprintf("您好：\r\n\r\n您正在通过 QQ 机器人绑定 %s 账户。\r\n验证码：%s\r\n验证码将在 %d 分钟后失效。\r\n\r\n若非本人操作，请忽略本邮件。\r\n", systemName, code, minutes)
	message := strings.Join([]string{
		"From: " + from.String(),
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		"Date: " + time.Now().Format(time.RFC1123Z),
		"",
		body,
	}, "\r\n")
	return s.send(ctx, from.Address, []string{to}, []byte(message))
}

func (s *SMTP) send(ctx context.Context, from string, recipients []string, message []byte) error {
	address := net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))
	dialer := &net.Dialer{Timeout: s.timeout}
	var conn net.Conn
	var err error
	if s.tlsMode == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", address, &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(s.timeout)
	_ = conn.SetDeadline(deadline)

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return fmt.Errorf("创建 SMTP 会话失败: %w", err)
	}
	defer client.Close()
	if s.tlsMode == "starttls" {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return fmt.Errorf("SMTP 服务器不支持 STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("SMTP STARTTLS 失败: %w", err)
		}
	}
	if s.username != "" {
		auth := smtp.PlainAuth("", s.username, s.password, s.host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("SMTP 发件人被拒绝: %w", err)
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("SMTP 收件人被拒绝: %w", err)
		}
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA 命令失败: %w", err)
	}
	bw := bufio.NewWriter(writer)
	if _, err := bw.Write(message); err != nil {
		_ = writer.Close()
		return fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err := bw.Flush(); err != nil {
		_ = writer.Close()
		return fmt.Errorf("刷新邮件内容失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("提交邮件失败: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("结束 SMTP 会话失败: %w", err)
	}
	return nil
}
