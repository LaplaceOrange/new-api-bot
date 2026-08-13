package resetradar

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestProxyValidationAndMask(t *testing.T) {
	valid := []string{
		"",
		"off",
		"http://127.0.0.1:8080",
		"http://user:secret@example.com:3128",
		"socks5://user:p%40ss@localhost:1080",
	}
	for _, value := range valid {
		if err := ValidateProxyURL(value); err != nil {
			t.Fatalf("ValidateProxyURL(%q): %v", value, err)
		}
	}
	invalid := []string{
		"https://localhost:8080",
		"socks5://localhost",
		"http://:8080",
		"http://localhost:70000",
		"http://localhost:8080/path",
		"http://localhost:8080/?token=secret",
	}
	for _, value := range invalid {
		if err := ValidateProxyURL(value); err == nil {
			t.Fatalf("ValidateProxyURL(%q) expected error", value)
		}
	}
	masked := MaskedProxy("socks5://alice:plain-secret@proxy.example:1080")
	if strings.Contains(masked, "plain-secret") || !strings.Contains(masked, "xxxxx") {
		t.Fatalf("proxy credentials were not masked: %q", masked)
	}
	if MaskedProxy("") != "off" || MaskedProxy("not-a-url") != "<invalid>" {
		t.Fatal("unexpected empty or invalid proxy mask")
	}
}

func TestProxyValidationErrorDoesNotExposeCredentials(t *testing.T) {
	values := []string{
		"http://alice:plain-secret@localhost:not-a-port",
		"http://alice:another-secret@local%zz:8080",
	}
	for _, value := range values {
		err := ValidateProxyURL(value)
		if err == nil {
			t.Fatalf("ValidateProxyURL(%q) expected error", value)
		}
		for _, secret := range []string{value, "alice", "plain-secret", "another-secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("validation error exposed %q: %q", secret, err)
			}
		}
	}
}

func TestSOCKS5DialerNegotiatesAuthenticatedDomain(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		greeting := make([]byte, 2)
		if _, err := io.ReadFull(reader, greeting); err != nil {
			serverErr <- err
			return
		}
		methods := make([]byte, int(greeting[1]))
		if _, err := io.ReadFull(reader, methods); err != nil {
			serverErr <- err
			return
		}
		if greeting[0] != 5 || len(methods) != 2 || methods[0] != 0 || methods[1] != 2 {
			serverErr <- &proxyTestError{"unexpected greeting"}
			return
		}
		if _, err := conn.Write([]byte{5, 2}); err != nil {
			serverErr <- err
			return
		}
		authHeader := make([]byte, 2)
		if _, err := io.ReadFull(reader, authHeader); err != nil {
			serverErr <- err
			return
		}
		username := make([]byte, int(authHeader[1]))
		if _, err := io.ReadFull(reader, username); err != nil {
			serverErr <- err
			return
		}
		passwordLength, err := reader.ReadByte()
		if err != nil {
			serverErr <- err
			return
		}
		password := make([]byte, int(passwordLength))
		if _, err := io.ReadFull(reader, password); err != nil {
			serverErr <- err
			return
		}
		if authHeader[0] != 1 || string(username) != "alice" || string(password) != "secret" {
			serverErr <- &proxyTestError{"unexpected credentials"}
			return
		}
		if _, err := conn.Write([]byte{1, 0}); err != nil {
			serverErr <- err
			return
		}
		requestHeader := make([]byte, 4)
		if _, err := io.ReadFull(reader, requestHeader); err != nil {
			serverErr <- err
			return
		}
		if requestHeader[0] != 5 || requestHeader[1] != 1 || requestHeader[3] != 3 {
			serverErr <- &proxyTestError{"unexpected connect request"}
			return
		}
		hostLength, err := reader.ReadByte()
		if err != nil {
			serverErr <- err
			return
		}
		host := make([]byte, int(hostLength))
		port := make([]byte, 2)
		if _, err := io.ReadFull(reader, host); err != nil {
			serverErr <- err
			return
		}
		if _, err := io.ReadFull(reader, port); err != nil {
			serverErr <- err
			return
		}
		if string(host) != "timeline.example" || binary.BigEndian.Uint16(port) != 443 {
			serverErr <- &proxyTestError{"unexpected target"}
			return
		}
		_, err = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0x1f, 0x90})
		serverErr <- err
	}()

	dialer := socks5Dialer{
		proxyAddress: listener.Addr().String(),
		username:     "alice",
		password:     "secret",
		timeout:      time.Second,
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "timeline.example:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

type proxyTestError struct{ message string }

func (e *proxyTestError) Error() string { return e.message }
