package rss

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testXML = `<rss><channel><title>Feed</title><item><guid>1</guid><title>Hello</title></item></channel></rss>`

func newTestClient(t *testing.T, timeout time.Duration, proxy string) *Client {
	t.Helper()
	client, err := NewClient(timeout, proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestClientConditionalRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"one"` && r.Header.Get("If-Modified-Since") == "yesterday" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Last-Modified", "yesterday")
		fmt.Fprint(w, testXML)
	}))
	defer server.Close()
	client := newTestClient(t, time.Second, "off")
	first, err := client.Fetch(context.Background(), server.URL, Validators{})
	if err != nil || len(first.Articles) != 1 || first.ETag != `"one"` {
		t.Fatal(first, err)
	}
	second, err := client.Fetch(context.Background(), server.URL, first.Validators)
	if err != nil || !second.NotModified || second.Validators != first.Validators {
		t.Fatal(second, err)
	}
}

func TestClientErrorsAreBoundedAndRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/large":
			fmt.Fprint(w, strings.Repeat("x", MaxBodyBytes+1))
		case "/bad":
			fmt.Fprint(w, "<html/>")
		case "/304":
			w.WriteHeader(http.StatusNotModified)
		case "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	for _, path := range []string{"/large", "/bad", "/304", "/slow", "/403"} {
		client := newTestClient(t, 100*time.Millisecond, "off")
		_, err := client.Fetch(context.Background(), server.URL+path+"?token=super-secret", Validators{})
		if err == nil || strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), server.URL) {
			t.Fatal(path, err)
		}
	}
}

func TestRedirectLimitAndCrossOriginHeaders(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("If-None-Match") != "" {
			t.Error("credentials/validators leaked across origins")
		}
		fmt.Fprint(w, testXML)
	}))
	defer target.Close()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cross" {
			http.Redirect(w, r, target.URL, http.StatusFound)
			return
		}
		var hops int
		fmt.Sscanf(r.URL.Path, "/%d", &hops)
		if hops > 0 {
			http.Redirect(w, r, fmt.Sprintf("/%d", hops-1), http.StatusFound)
			return
		}
		fmt.Fprint(w, testXML)
	}))
	defer server.Close()
	client := newTestClient(t, time.Second, "off")
	for _, hops := range []int{5, 6} {
		_, err := client.Fetch(context.Background(), fmt.Sprintf("%s/%d", server.URL, hops), Validators{})
		if (hops == 6) != (err != nil) {
			t.Fatal(hops, err)
		}
	}
	address := strings.Replace(server.URL, "http://", "http://alice:secret@", 1) + "/cross"
	if _, err := client.Fetch(context.Background(), address, Validators{ETag: `"one"`}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPAndHTTPSProxyAuthenticationNoFallback(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
				if r.Header.Get("Proxy-Authorization") != want || r.URL.Hostname() != "fixture.invalid" {
					t.Error("missing proxy authentication or wrong target")
				}
				fmt.Fprint(w, testXML)
			})
			var proxy *httptest.Server
			if secure {
				proxy = httptest.NewTLSServer(handler)
			} else {
				proxy = httptest.NewServer(handler)
			}
			defer proxy.Close()
			address := strings.Replace(proxy.URL, "://", "://alice:secret@", 1)
			client := newTestClient(t, time.Second, address)
			if secure {
				pool := x509.NewCertPool()
				pool.AddCert(proxy.Certificate())
				client.http.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			}
			if _, err := client.Fetch(context.Background(), "http://fixture.invalid/rss", Validators{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) }))
	defer proxy.Close()
	originCalls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { originCalls++; fmt.Fprint(w, testXML) }))
	defer origin.Close()
	client := newTestClient(t, time.Second, proxy.URL)
	if _, err := client.Fetch(context.Background(), origin.URL, Validators{}); err == nil || originCalls != 0 {
		t.Fatal("proxy failure fell back to direct", err, originCalls)
	}
}

func TestSOCKSProxyAuthentication(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(2 * time.Second))
				read := func(n int) []byte {
					value := make([]byte, n)
					_, err = io.ReadFull(conn, value)
					return value
				}
				greeting := read(2)
				read(int(greeting[1]))
				conn.Write([]byte{5, 2})
				auth := read(2)
				username := read(int(auth[1]))
				passwordLength := read(1)[0]
				password := read(int(passwordLength))
				if string(username) != "alice" || string(password) != "secret" {
					done <- fmt.Errorf("bad SOCKS authentication")
					return
				}
				conn.Write([]byte{1, 0})
				request := read(4)
				if request[3] != 3 {
					done <- fmt.Errorf("expected proxy-resolved domain")
					return
				}
				hostLength := read(1)[0]
				host := read(int(hostLength))
				read(2)
				if err != nil || string(host) != "fixture.invalid" {
					done <- fmt.Errorf("bad target: %s (%v)", host, err)
					return
				}
				conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err == nil {
					req.Body.Close()
					_, err = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(testXML), testXML)
				}
				done <- err
			}()
			client := newTestClient(t, time.Second, scheme+"://alice:secret@"+listener.Addr().String())
			if _, err := client.Fetch(context.Background(), "http://fixture.invalid/rss", Validators{}); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestURLAndProxyValidation(t *testing.T) {
	for _, value := range []string{"file:///x", "ftp://host/rss", "http://", "http://host:0", "http://host:99999", "http://host/%zz"} {
		if _, err := NormalizeURL(value); err == nil {
			t.Fatal(value)
		}
	}
	if got, err := NormalizeURL("https://EXAMPLE.com#fragment"); err != nil || got != "https://example.com/" {
		t.Fatal(got, err)
	}
	for _, value := range []string{"ftp://proxy", "http://proxy:0", "http://proxy/path?token=secret", "socks5://proxy:99999"} {
		if err := ValidateProxyURL(value); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(value, err)
		}
	}
}
