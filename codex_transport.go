package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	xproxy "golang.org/x/net/proxy"
)

var codexChromeTransports sync.Map

// codexOAuthRoundTripper avoids chatgpt.com's Cloudflare challenge for Go's
// default TLS fingerprint while preserving caller-injected transports.
func codexOAuthRoundTripper(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	transport, ok := base.(*http.Transport)
	if !ok || transport == nil {
		return base
	}
	if cached, ok := codexChromeTransports.Load(transport); ok {
		return cached.(http.RoundTripper)
	}
	created := newCodexChromeTransport(transport)
	actual, _ := codexChromeTransports.LoadOrStore(transport, created)
	return actual.(http.RoundTripper)
}

func newCodexChromeTransport(base *http.Transport) *http2.Transport {
	dialContext := base.DialContext
	if dialContext == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dialContext = dialer.DialContext
	}
	return &http2.Transport{
		DisableCompression: base.DisableCompression,
		IdleConnTimeout:    base.IdleConnTimeout,
		DialTLSContext: func(ctx context.Context, network, addr string, config *tls.Config) (net.Conn, error) {
			plain, err := dialCodexTarget(ctx, network, addr, dialContext, base.Proxy)
			if err != nil {
				return nil, err
			}
			serverName := ""
			if config != nil {
				serverName = config.ServerName
			}
			if serverName == "" {
				serverName, _, err = net.SplitHostPort(addr)
				if err != nil {
					_ = plain.Close()
					return nil, err
				}
			}
			secure := utls.UClient(plain, &utls.Config{
				ServerName: serverName,
				MinVersion: tls.VersionTLS12,
			}, utls.HelloChrome_Auto)
			if err = secure.HandshakeContext(ctx); err != nil {
				_ = plain.Close()
				return nil, err
			}
			if secure.ConnectionState().NegotiatedProtocol != http2.NextProtoTLS {
				_ = secure.Close()
				return nil, fmt.Errorf("codex upstream did not negotiate HTTP/2")
			}
			return secure, nil
		},
	}
}

func dialCodexWebSocketTarget(ctx context.Context, target *url.URL, base *http.Transport) (net.Conn, error) {
	if target == nil || !strings.EqualFold(target.Scheme, "wss") || target.Hostname() == "" {
		return nil, fmt.Errorf("invalid Codex WebSocket target")
	}
	if base == nil {
		return nil, fmt.Errorf("Codex WebSocket transport is unavailable")
	}
	dialContext := base.DialContext
	if dialContext == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dialContext = dialer.DialContext
	}
	address := target.Host
	if target.Port() == "" {
		address = net.JoinHostPort(target.Hostname(), "443")
	}
	plain, err := dialCodexTarget(ctx, "tcp", address, dialContext, base.Proxy)
	if err != nil {
		return nil, err
	}

	serverName := target.Hostname()
	secureConfig := &utls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if base.TLSClientConfig != nil {
		secureConfig.RootCAs = base.TLSClientConfig.RootCAs
		secureConfig.InsecureSkipVerify = base.TLSClientConfig.InsecureSkipVerify
		secureConfig.MaxVersion = base.TLSClientConfig.MaxVersion
		if base.TLSClientConfig.MinVersion != 0 {
			secureConfig.MinVersion = base.TLSClientConfig.MinVersion
		}
		if base.TLSClientConfig.ServerName != "" {
			secureConfig.ServerName = base.TLSClientConfig.ServerName
		}
	}
	secure := utls.UClient(plain, secureConfig, utls.HelloChrome_Auto)
	if err = secure.BuildHandshakeState(); err != nil {
		_ = plain.Close()
		return nil, err
	}
	extensions := secure.Extensions[:0]
	for _, extension := range secure.Extensions {
		switch typed := extension.(type) {
		case *utls.ALPNExtension:
			typed.AlpnProtocols = []string{"http/1.1"}
			extensions = append(extensions, typed)
		case *utls.ApplicationSettingsExtension, *utls.ApplicationSettingsExtensionNew:
			continue
		default:
			extensions = append(extensions, extension)
		}
	}
	secure.Extensions = extensions
	if err = secure.HandshakeContext(ctx); err != nil {
		_ = plain.Close()
		return nil, err
	}
	if protocol := secure.ConnectionState().NegotiatedProtocol; protocol != "" && protocol != "http/1.1" {
		_ = secure.Close()
		return nil, fmt.Errorf("Codex WebSocket negotiated unsupported protocol %q", protocol)
	}
	return secure, nil
}

type codexContextDialer struct {
	ctx  context.Context
	dial func(context.Context, string, string) (net.Conn, error)
}

func (dialer codexContextDialer) Dial(network, addr string) (net.Conn, error) {
	return dialer.dial(dialer.ctx, network, addr)
}

func (dialer codexContextDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return dialer.dial(ctx, network, addr)
}

func dialCodexTarget(
	ctx context.Context,
	network string,
	addr string,
	dialContext func(context.Context, string, string) (net.Conn, error),
	proxyFor func(*http.Request) (*url.URL, error),
) (net.Conn, error) {
	if proxyFor == nil {
		return dialContext(ctx, network, addr)
	}
	proxyURL, err := proxyFor(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	if err != nil {
		return nil, err
	}
	if proxyURL == nil {
		return dialContext(ctx, network, addr)
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https":
		return dialCodexHTTPProxy(ctx, network, addr, proxyURL, dialContext)
	case "socks5", "socks5h":
		proxyDialer, err := xproxy.FromURL(proxyURL, codexContextDialer{ctx: ctx, dial: dialContext})
		if err != nil {
			return nil, err
		}
		if contextDialer, ok := proxyDialer.(xproxy.ContextDialer); ok {
			return contextDialer.DialContext(ctx, network, addr)
		}
		return proxyDialer.Dial(network, addr)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", proxyURL.Scheme)
	}
}

func dialCodexHTTPProxy(
	ctx context.Context,
	network string,
	addr string,
	proxyURL *url.URL,
	dialContext func(context.Context, string, string) (net.Conn, error),
) (net.Conn, error) {
	connection, err := dialContext(ctx, network, codexProxyAddress(proxyURL))
	if err != nil {
		return nil, err
	}
	plainConnection := connection
	stopContextClose := context.AfterFunc(ctx, func() {
		_ = plainConnection.Close()
	})
	defer stopContextClose()
	if strings.EqualFold(proxyURL.Scheme, "https") {
		proxyTLS := tls.Client(connection, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: proxyURL.Hostname(),
		})
		if err = proxyTLS.HandshakeContext(ctx); err != nil {
			_ = connection.Close()
			return nil, err
		}
		connection = proxyTLS
	}
	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	}
	if err = request.Write(connection); err != nil {
		_ = connection.Close()
		return nil, err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = connection.Close()
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("proxy CONNECT returned %s", response.Status)
	}
	if !stopContextClose() {
		_ = connection.Close()
		if err = ctx.Err(); err == nil {
			err = context.Canceled
		}
		return nil, err
	}
	if reader.Buffered() == 0 {
		return connection, nil
	}
	return &codexBufferedConn{Conn: connection, reader: reader}, nil
}

func codexProxyAddress(proxyURL *url.URL) string {
	port := proxyURL.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(proxyURL.Scheme, "https") {
			port = "443"
		}
	}
	return net.JoinHostPort(proxyURL.Hostname(), port)
}

type codexBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (connection *codexBufferedConn) Read(buffer []byte) (int, error) {
	if connection.reader.Buffered() != 0 {
		return connection.reader.Read(buffer)
	}
	return connection.Conn.Read(buffer)
}
