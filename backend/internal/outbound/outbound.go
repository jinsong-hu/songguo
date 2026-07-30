// Package outbound owns provider-facing network connections. It gives every
// caller the same direct/HTTPS-proxy/SOCKS5 behavior without consulting process
// HTTP_PROXY or HTTPS_PROXY environment variables.
package outbound

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/songguo/songguo/internal/config"
)

type Options struct {
	// DirectClient is an optional test hook. Production leaves it nil so direct
	// traffic always uses a transport with Proxy unset.
	DirectClient *http.Client
	// TLSConfig is an optional test/private-CA hook used for TLS to providers
	// and HTTPS proxies. Production normally leaves it nil.
	TLSConfig *tls.Config
}

// Manager caches connection pools by the full proxy configuration. Reset drops
// those caches after a config reload; active requests keep their existing
// transport while later requests pick up the new settings.
type Manager struct {
	mu             sync.Mutex
	clients        map[string]*http.Client
	directOverride *http.Client
	tlsConfig      *tls.Config
}

func New(opts Options) *Manager {
	return &Manager{
		clients:        make(map[string]*http.Client),
		directOverride: opts.DirectClient,
		tlsConfig:      opts.TLSConfig,
	}
}

func (m *Manager) Do(req *http.Request, p *config.Proxy) (*http.Response, error) {
	return m.Client(p).Do(req)
}

// Client returns a streaming-safe HTTP client for p. A nil proxy is direct.
func (m *Manager) Client(p *config.Proxy) *http.Client {
	if p == nil && m.directOverride != nil {
		return m.directOverride
	}
	key := proxyKey(p)
	m.mu.Lock()
	defer m.mu.Unlock()
	if client := m.clients[key]; client != nil {
		return client
	}
	client := &http.Client{Transport: newTransport(p, m.tlsConfig)}
	m.clients[key] = client
	return client
}

// Reset closes idle pooled connections and forgets cached transports. It does
// not interrupt active requests or streams.
func (m *Manager) Reset() {
	m.mu.Lock()
	clients := m.clients
	m.clients = make(map[string]*http.Client)
	direct := m.directOverride
	m.mu.Unlock()

	for _, client := range clients {
		client.CloseIdleConnections()
	}
	if direct != nil {
		direct.CloseIdleConnections()
	}
}

func newTransport(p *config.Proxy, tlsConfig *tls.Config) *http.Transport {
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 1 * time.Hour,
	}
	if tlsConfig != nil {
		t.TLSClientConfig = tlsConfig.Clone()
	}
	if p != nil {
		t.Proxy = http.ProxyURL(proxyURL(p))
	}
	return t
}

func proxyURL(p *config.Proxy) *url.URL {
	u := &url.URL{
		Scheme: p.Type,
		Host:   net.JoinHostPort(strings.Trim(p.Host, "[]"), strconv.Itoa(p.Port)),
	}
	if p.Username != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u
}

func proxyKey(p *config.Proxy) string {
	if p == nil {
		return "direct"
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		p.ID, p.Type, p.Host, strconv.Itoa(p.Port), p.Username, p.Password,
	}, "\x00")))
	return fmt.Sprintf("%x", sum[:])
}

// DialContext opens a TCP connection to address through p. The returned
// connection is a plain tunnel; callers layer upstream TLS on it when needed.
func (m *Manager) DialContext(ctx context.Context, p *config.Proxy, network, address string) (net.Conn, error) {
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if p == nil {
		return direct.DialContext(ctx, network, address)
	}

	proxyAddress := net.JoinHostPort(strings.Trim(p.Host, "[]"), strconv.Itoa(p.Port))
	switch p.Type {
	case config.ProxySOCKS5:
		var auth *xproxy.Auth
		if p.Username != "" {
			auth = &xproxy.Auth{User: p.Username, Password: p.Password}
		}
		dialer, err := xproxy.SOCKS5("tcp", proxyAddress, auth, direct)
		if err != nil {
			return nil, fmt.Errorf("configure SOCKS5 proxy %q: %w", p.Name, err)
		}
		if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
			return contextDialer.DialContext(ctx, network, address)
		}
		return dialWithContext(ctx, dialer, network, address)
	case config.ProxyHTTPS:
		return dialHTTPSProxy(ctx, direct, p, proxyAddress, address, m.tlsConfig)
	default:
		return nil, fmt.Errorf("proxy %q has unsupported type %q", p.Name, p.Type)
	}
}

func dialWithContext(ctx context.Context, dialer xproxy.Dialer, network, address string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := dialer.Dial(network, address)
		done <- result{conn: conn, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-done:
		if ctx.Err() != nil && result.conn != nil {
			_ = result.conn.Close()
			return nil, ctx.Err()
		}
		return result.conn, result.err
	}
}

func dialHTTPSProxy(ctx context.Context, direct *net.Dialer, p *config.Proxy, proxyAddress, target string, baseTLSConfig *tls.Config) (net.Conn, error) {
	raw, err := direct.DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("dial HTTPS proxy %q: %w", p.Name, err)
	}
	fail := func(err error) (net.Conn, error) {
		_ = raw.Close()
		return nil, err
	}

	deadline := time.Now().Add(10 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = raw.SetDeadline(deadline)

	tlsConfig := &tls.Config{}
	if baseTLSConfig != nil {
		tlsConfig = baseTLSConfig.Clone()
	}
	tlsConfig.ServerName = strings.Trim(p.Host, "[]")
	tlsConn := tls.Client(raw, tlsConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fail(fmt.Errorf("TLS handshake with HTTPS proxy %q: %w", p.Name, err))
	}

	var request strings.Builder
	fmt.Fprintf(&request, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if p.Username != "" {
		token := base64.StdEncoding.EncodeToString([]byte(p.Username + ":" + p.Password))
		fmt.Fprintf(&request, "Proxy-Authorization: Basic %s\r\n", token)
	}
	request.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")
	if _, err := io.WriteString(tlsConn, request.String()); err != nil {
		return fail(fmt.Errorf("write CONNECT to HTTPS proxy %q: %w", p.Name, err))
	}

	reader := bufio.NewReader(tlsConn)
	connectReq := &http.Request{Method: http.MethodConnect}
	resp, err := http.ReadResponse(reader, connectReq)
	if err != nil {
		return fail(fmt.Errorf("read CONNECT response from HTTPS proxy %q: %w", p.Name, err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		_ = resp.Body.Close()
		return fail(fmt.Errorf("HTTPS proxy %q CONNECT returned %s", p.Name, resp.Status))
	}

	_ = tlsConn.SetDeadline(time.Time{})
	if reader.Buffered() == 0 {
		return tlsConn, nil
	}
	return &bufferedConn{Conn: tlsConn, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
