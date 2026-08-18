package resetradar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func ValidateProxyURL(raw string) error {
	_, err := parseProxyURL(raw)
	return err
}

func MaskedProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "off") {
		return "off"
	}
	proxyURL, err := parseProxyURL(raw)
	if err != nil {
		return "<invalid>"
	}
	return proxyURL.Redacted()
}

func parseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "off") {
		return nil, nil
	}
	proxyURL, err := url.Parse(raw)
	if err != nil {
		// url.Parse errors can echo the complete input, including credentials.
		return nil, errors.New("解析代理链接失败，请检查格式")
	}
	proxyURL.Scheme = strings.ToLower(proxyURL.Scheme)
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "socks5" {
		return nil, errors.New("代理链接仅支持 http:// 或 socks5://")
	}
	if proxyURL.Hostname() == "" || proxyURL.Port() == "" {
		return nil, errors.New("代理链接必须包含主机和端口")
	}
	port, err := strconv.Atoi(proxyURL.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("代理端口必须是 1 到 65535 的整数")
	}
	if proxyURL.Path != "" && proxyURL.Path != "/" || proxyURL.RawQuery != "" || proxyURL.Fragment != "" {
		return nil, errors.New("代理链接不能包含路径、查询参数或片段")
	}
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		if len(username) > 255 || len(password) > 255 {
			return nil, errors.New("代理用户名和密码不能超过 255 字节")
		}
	}
	return proxyURL, nil
}

type socks5Dialer struct {
	proxyAddress string
	username     string
	password     string
	timeout      time.Duration
}

func (d socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SOCKS5 不支持网络类型 %q", network)
	}
	conn, err := (&net.Dialer{Timeout: d.timeout, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", d.proxyAddress)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	deadline := time.Now().Add(d.timeout)
	if contextDeadline, hasDeadline := ctx.Deadline(); hasDeadline && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if err := d.negotiate(conn, address); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ok = true
	return conn, nil
}

func (d socks5Dialer) negotiate(conn net.Conn, address string) error {
	methods := []byte{0x00}
	if d.username != "" || d.password != "" {
		methods = append(methods, 0x02)
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if err := writeAll(conn, greeting); err != nil {
		return fmt.Errorf("发送 SOCKS5 握手失败: %w", err)
	}
	response := make([]byte, 2)
	if _, err := io.ReadFull(conn, response); err != nil {
		return fmt.Errorf("读取 SOCKS5 握手失败: %w", err)
	}
	if response[0] != 0x05 || response[1] == 0xff {
		return errors.New("SOCKS5 代理未接受认证方式")
	}
	if response[1] == 0x02 {
		if d.username == "" && d.password == "" {
			return errors.New("SOCKS5 代理要求用户名密码")
		}
		auth := []byte{0x01, byte(len(d.username))}
		auth = append(auth, d.username...)
		auth = append(auth, byte(len(d.password)))
		auth = append(auth, d.password...)
		if err := writeAll(conn, auth); err != nil {
			return fmt.Errorf("发送 SOCKS5 认证信息失败: %w", err)
		}
		if _, err := io.ReadFull(conn, response); err != nil {
			return fmt.Errorf("读取 SOCKS5 认证结果失败: %w", err)
		}
		if response[0] != 0x01 || response[1] != 0x00 {
			return errors.New("SOCKS5 用户名或密码验证失败")
		}
	} else if response[1] != 0x00 {
		return fmt.Errorf("SOCKS5 返回未知认证方式 0x%02x", response[1])
	}

	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("解析 SOCKS5 目标地址失败: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("SOCKS5 目标端口无效")
	}
	request := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			request = append(request, 0x01)
			request = append(request, ipv4...)
		} else {
			request = append(request, 0x04)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) == 0 || len(host) > 255 {
			return errors.New("SOCKS5 目标域名长度无效")
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, host...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	request = append(request, portBytes...)
	if err := writeAll(conn, request); err != nil {
		return fmt.Errorf("发送 SOCKS5 连接请求失败: %w", err)
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("读取 SOCKS5 连接结果失败: %w", err)
	}
	if header[0] != 0x05 || header[1] != 0x00 {
		return fmt.Errorf("SOCKS5 代理拒绝连接，状态码 0x%02x", header[1])
	}
	addressLength := 0
	switch header[3] {
	case 0x01:
		addressLength = net.IPv4len
	case 0x04:
		addressLength = net.IPv6len
	case 0x03:
		length := []byte{0}
		if _, err := io.ReadFull(conn, length); err != nil {
			return fmt.Errorf("读取 SOCKS5 返回地址失败: %w", err)
		}
		addressLength = int(length[0])
	default:
		return errors.New("SOCKS5 返回了未知地址类型")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(addressLength+2)); err != nil {
		return fmt.Errorf("读取 SOCKS5 返回地址失败: %w", err)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
