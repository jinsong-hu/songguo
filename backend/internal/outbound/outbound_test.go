package outbound

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/config"
)

func TestSOCKS5HTTPClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "through-socks")
	}))
	defer upstream.Close()

	proxyAddress, authenticated := startSOCKS5Proxy(t, "alice", "secret")
	host, port := splitAddress(t, proxyAddress)
	manager := New(Options{})
	defer manager.Reset()

	resp, err := manager.Client(&config.Proxy{
		ID: "p1", Name: "socks", Type: config.ProxySOCKS5,
		Host: host, Port: port, Username: "alice", Password: "secret",
	}).Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET through SOCKS5: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "through-socks" {
		t.Fatalf("body = %q, want through-socks", body)
	}
	if !authenticated.Load() {
		t.Fatal("SOCKS5 username/password authentication was not used")
	}
}

func TestHTTPSProxyConnectDial(t *testing.T) {
	echoAddress := startEchoServer(t)
	var authenticated atomic.Bool

	proxyServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:hunter2"))
		if r.Header.Get("Proxy-Authorization") != wantAuth {
			w.Header().Set("Proxy-Authenticate", `Basic realm="test"`)
			http.Error(w, "proxy auth required", http.StatusProxyAuthRequired)
			return
		}
		authenticated.Store(true)

		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "dial failed", http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
			return
		}
		client, rw, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		go func() {
			defer client.Close()
			defer upstream.Close()
			_, _ = io.Copy(upstream, client)
		}()
		go func() {
			defer client.Close()
			defer upstream.Close()
			_, _ = io.Copy(client, upstream)
		}()
	}))
	defer proxyServer.Close()

	proxyURL, _ := url.Parse(proxyServer.URL)
	host, port := splitAddress(t, proxyURL.Host)
	tlsConfig := proxyServer.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	manager := New(Options{TLSConfig: tlsConfig})
	defer manager.Reset()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := manager.DialContext(ctx, &config.Proxy{
		ID: "p2", Name: "secure", Type: config.ProxyHTTPS,
		Host: host, Port: port, Username: "bob", Password: "hunter2",
	}, "tcp", echoAddress)
	if err != nil {
		t.Fatalf("DialContext through HTTPS proxy: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write tunnel: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read tunnel: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
	if !authenticated.Load() {
		t.Fatal("HTTPS proxy Basic authentication was not used")
	}
}

func TestDirectTransportHasNoEnvironmentProxy(t *testing.T) {
	transport := newTransport(nil, nil)
	if transport.Proxy != nil {
		t.Fatal("direct transport must not consult environment proxy variables")
	}
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						_, _ = conn.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func startSOCKS5Proxy(t *testing.T, wantUser, wantPassword string) (string, *atomic.Bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen SOCKS5: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	authenticated := &atomic.Bool{}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSOCKS5Conn(conn, wantUser, wantPassword, authenticated)
		}
	}()
	return listener.Addr().String(), authenticated
}

func serveSOCKS5Conn(conn net.Conn, wantUser, wantPassword string, authenticated *atomic.Bool) {
	defer conn.Close()
	reader := bufio.NewReader(conn)

	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}
	method := byte(0)
	if wantUser != "" {
		method = 2
	}
	if _, err := conn.Write([]byte{5, method}); err != nil {
		return
	}

	if method == 2 {
		if _, err := io.ReadFull(reader, header); err != nil || header[0] != 1 {
			return
		}
		username := make([]byte, int(header[1]))
		if _, err := io.ReadFull(reader, username); err != nil {
			return
		}
		length, err := reader.ReadByte()
		if err != nil {
			return
		}
		password := make([]byte, int(length))
		if _, err := io.ReadFull(reader, password); err != nil {
			return
		}
		if string(username) != wantUser || string(password) != wantPassword {
			_, _ = conn.Write([]byte{1, 1})
			return
		}
		authenticated.Store(true)
		if _, err := conn.Write([]byte{1, 0}); err != nil {
			return
		}
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(reader, request); err != nil || request[0] != 5 || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		raw := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	case 3:
		length, err := reader.ReadByte()
		if err != nil {
			return
		}
		raw := make([]byte, int(length))
		if _, err := io.ReadFull(reader, raw); err != nil {
			return
		}
		host = string(raw)
	case 4:
		raw := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes))))
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(upstream, reader)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		done <- struct{}{}
	}()
	<-done
}

func splitAddress(t *testing.T, address string) (string, int) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split address %q: %v", address, err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("parse port %q: %v", rawPort, err)
	}
	return host, port
}
