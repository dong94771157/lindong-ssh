package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// ---------------- 网络代理（SSH 与 AI API/云同步 分流） ----------------

type ProxyConfig struct {
	SSHProxy string `json:"sshProxy"` // SSH 连接走此代理：http://user:pass@host:port 或 socks5://[user:pass@]host:port
	APIProxy string `json:"apiProxy"` // AI 接口与云同步请求走此代理（格式同上）
}

type proxyStoreT struct {
	mu   sync.RWMutex
	path string
	cfg  ProxyConfig
}

func newProxyStore(path string) *proxyStoreT {
	s := &proxyStoreT{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.cfg)
	}
	return s
}

var proxySt = newProxyStore(dataFile("proxy.json"))

func (a *App) GetProxyConfig() ProxyConfig {
	proxySt.mu.RLock()
	defer proxySt.mu.RUnlock()
	return proxySt.cfg
}

func (a *App) SaveProxyConfig(c ProxyConfig) error {
	c.SSHProxy = strings.TrimSpace(c.SSHProxy)
	c.APIProxy = strings.TrimSpace(c.APIProxy)
	for _, p := range []string{c.SSHProxy, c.APIProxy} {
		if p == "" {
			continue
		}
		u, err := url.Parse(p)
		if err != nil || u.Host == "" {
			return errors.New("代理地址格式不正确，示例：http://127.0.0.1:7890 或 socks5://127.0.0.1:1080")
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5":
		default:
			return errors.New("仅支持 http/https/socks5 代理协议")
		}
	}
	proxySt.mu.Lock()
	defer proxySt.mu.Unlock()
	proxySt.cfg = c
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(proxySt.path, data, 0644)
}

func proxyCfg() ProxyConfig {
	proxySt.mu.RLock()
	defer proxySt.mu.RUnlock()
	return proxySt.cfg
}

// apiHTTPClient 返回按 API 代理配置的 http.Client（AI 接口与云同步共用）
func apiHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{}
	if p := strings.TrimSpace(proxyCfg().APIProxy); p != "" {
		if u, err := url.Parse(p); err == nil && u.Host != "" {
			// net/http 原生支持 http/https/socks5 代理 URL
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// bufferedConn 处理 HTTP CONNECT 后 bufio.Reader 已预读的字节（如 SSH 服务器 banner）
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// dialViaSSHProxy 按 SSH 代理配置建立到 addr 的 TCP 连接；未配置代理时直连
func dialViaSSHProxy(addr string, timeout time.Duration) (net.Conn, error) {
	p := strings.TrimSpace(proxyCfg().SSHProxy)
	if p == "" {
		return net.DialTimeout("tcp", addr, timeout)
	}
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return nil, errors.New("SSH 代理地址格式不正确")
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, &net.Dialer{Timeout: timeout})
		if err != nil {
			return nil, fmt.Errorf("SOCKS5 代理初始化失败: %v", err)
		}
		return d.Dial("tcp", addr)
	case "http", "https":
		conn, err := net.DialTimeout("tcp", u.Host, timeout)
		if err != nil {
			return nil, fmt.Errorf("连接 HTTP 代理失败: %v", err)
		}
		_ = conn.SetDeadline(time.Now().Add(timeout))
		var sb strings.Builder
		sb.WriteString("CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n")
		if u.User != nil {
			pw, _ := u.User.Password()
			cred := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pw))
			sb.WriteString("Proxy-Authorization: Basic " + cred + "\r\n")
		}
		sb.WriteString("\r\n")
		if _, err := conn.Write([]byte(sb.String())); err != nil {
			conn.Close()
			return nil, fmt.Errorf("代理握手失败: %v", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("读取代理响应失败: %v", err)
		}
		if resp.StatusCode != 200 {
			conn.Close()
			return nil, fmt.Errorf("代理拒绝连接：%s", resp.Status)
		}
		_ = conn.SetDeadline(time.Time{})
		return &bufferedConn{Conn: conn, r: br}, nil
	default:
		return nil, errors.New("SSH 代理仅支持 http/https/socks5 协议")
	}
}
