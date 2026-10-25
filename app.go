package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/crypto/ssh"
)

// ---------------- 数据模型与存储 ----------------

type Server struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"` // "ssh"（默认）或 "rdp"（Windows 远程桌面）
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	PrivateKey string `json:"privateKey"`
	CreatedAt  int64  `json:"createdAt"`
}

type serverStore struct {
	mu      sync.RWMutex
	path    string
	Servers []Server `json:"servers"`
}

func newServerStore(path string) *serverStore {
	s := &serverStore{path: path, Servers: []Server{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, s)
	}
	return s
}

func (s *serverStore) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (s *serverStore) list() []Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Server, len(s.Servers))
	copy(out, s.Servers)
	return out
}

func (s *serverStore) get(id string) (Server, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sv := range s.Servers {
		if sv.ID == id {
			return sv, true
		}
	}
	return Server{}, false
}

func (s *serverStore) add(sv Server) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sv.ID = randomID()
	sv.CreatedAt = time.Now().Unix()
	s.Servers = append(s.Servers, sv)
	return sv, s.save()
}

func (s *serverStore) update(id string, sv Server) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.Servers {
		if old.ID == id {
			sv.ID = id
			sv.CreatedAt = old.CreatedAt
			s.Servers[i] = sv
			return sv, s.save()
		}
	}
	return Server{}, errors.New("服务器不存在")
}

func (s *serverStore) del(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sv := range s.Servers {
		if sv.ID == id {
			s.Servers = append(s.Servers[:i], s.Servers[i+1:]...)
			return s.save()
		}
	}
	return errors.New("服务器不存在")
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------- SSH 会话 ----------------

const maxPendingBuf = 512 * 1024
const maxRecentOut = 64 * 1024 // 终端最近输出环形缓冲上限（供 Agent read_terminal 使用）

type SSHSession struct {
	id      string
	name    string
	client  *ssh.Client
	session *ssh.Session
	stdin   io.WriteCloser

	srv         Server    // 连接来源服务器信息（用于连接日志）
	connectedAt time.Time // 建连成功时刻（用于计算会话时长）

	mu        sync.Mutex
	attached  bool
	closed    bool
	exited    bool
	buf       []byte // 前端订阅事件前缓存输出，防止丢失登录横幅
	recentOut []byte // 最近输出环形缓冲（上限 64KB）
}

// ---------------- App ----------------

type App struct {
	ctx      context.Context
	store    *serverStore
	logs     *logStore
	mu       sync.Mutex
	sessions map[string]*SSHSession

	agentMu    sync.Mutex
	agentCalls map[string]*int             // Agent 工具调用计数（按 agentID，子代理共享）
	askChans   map[string]chan askResponse // ask_user 阻塞等待通道
}

func NewApp() *App {
	return &App{
		sessions:   map[string]*SSHSession{},
		agentCalls: map[string]*int{},
		askChans:   map[string]chan askResponse{},
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.store = newServerStore(dataFile("servers.json"))
	a.logs = newLogStore(dataFile("logs.json"))
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	list := make([]*SSHSession, 0, len(a.sessions))
	for _, s := range a.sessions {
		list = append(list, s)
	}
	a.mu.Unlock()
	for _, s := range list {
		a.closeSession(s)
	}
}

// ---- 服务器管理（供前端调用） ----

func (a *App) GetServers() []Server {
	return a.store.list()
}

// normalizeServer 统一服务器类型与端口：type 仅接受 ssh/rdp（空值视为 ssh）
func normalizeServer(sv *Server) {
	if sv.Type != "rdp" {
		sv.Type = "ssh"
	}
	if sv.Port <= 0 {
		if sv.Type == "rdp" {
			sv.Port = 3389
		} else {
			sv.Port = 22
		}
	}
}

func (a *App) AddServer(sv Server) (Server, error) {
	if sv.Name == "" || sv.Host == "" || sv.Username == "" {
		return Server{}, errors.New("名称、主机、用户名不能为空")
	}
	normalizeServer(&sv)
	return a.store.add(sv)
}

func (a *App) UpdateServer(id string, sv Server) (Server, error) {
	if sv.Name == "" || sv.Host == "" || sv.Username == "" {
		return Server{}, errors.New("名称、主机、用户名不能为空")
	}
	normalizeServer(&sv)
	return a.store.update(id, sv)
}

func (a *App) DeleteServer(id string) error {
	return a.store.del(id)
}

// TcpPing 探测到主机端口的 TCP 建连延迟（毫秒），失败返回 -1
func (a *App) TcpPing(host string, port int) int {
	if host == "" {
		return -1
	}
	if port <= 0 {
		port = 22
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)), 3*time.Second)
	if err != nil {
		return -1
	}
	_ = conn.Close()
	return int(time.Since(start).Milliseconds())
}

// ---- SSH 连接（供前端调用） ----

type ConnectRequest struct {
	ServerID string `json:"serverId"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
}

type ConnectResult struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
}

func (a *App) Connect(req ConnectRequest) (ConnectResult, error) {
	var sv Server
	if req.ServerID != "" {
		found, ok := a.store.get(req.ServerID)
		if !ok {
			return ConnectResult{}, errors.New("服务器不存在")
		}
		sv = found
	} else {
		sv = Server{
			Name:     req.Host,
			Host:     req.Host,
			Port:     req.Port,
			Username: req.Username,
			Password: req.Password,
		}
	}

	sess, err := a.newSSHSession(sv, req.Cols, req.Rows)
	if err != nil {
		a.addLog(ConnLog{
			ServerID: sv.ID, Name: sv.Name, Host: sv.Host, Port: sv.Port,
			Username: sv.Username, Protocol: "ssh", Event: "connect",
			Status: "fail", Message: err.Error(),
		})
		return ConnectResult{}, err
	}
	a.addLog(ConnLog{
		ServerID: sv.ID, Name: sv.Name, Host: sv.Host, Port: sv.Port,
		Username: sv.Username, Protocol: "ssh", Event: "connect",
		Status: "success", Message: "连接成功",
	})
	return ConnectResult{SessionID: sess.id, Name: sv.Name}, nil
}

// Attach 前端订阅事件后调用，补发缓存的初始输出
func (a *App) Attach(id string) error {
	s := a.getSession(id)
	if s == nil {
		return errors.New("会话不存在")
	}
	s.mu.Lock()
	pending := string(s.buf)
	s.buf = nil
	s.attached = true
	exited := s.exited
	closed := s.closed
	s.mu.Unlock()

	if pending != "" {
		wailsRuntime.EventsEmit(a.ctx, "ssh:out:"+id, pending)
	}
	if exited || closed {
		wailsRuntime.EventsEmit(a.ctx, "ssh:exit:"+id)
	}
	return nil
}

func (a *App) Input(id, data string) {
	s := a.getSession(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		_, _ = s.stdin.Write([]byte(data))
	}
}

func (a *App) Resize(id string, cols, rows int) {
	s := a.getSession(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && cols > 0 && rows > 0 {
		_ = s.session.WindowChange(rows, cols)
	}
}

func (a *App) CloseSession(id string) {
	s := a.getSession(id)
	if s != nil {
		a.closeSession(s)
	}
}

// ---- 内部实现 ----

func (a *App) getSession(id string) *SSHSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

func (a *App) closeSession(s *SSHSession) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	_ = s.session.Close()
	_ = s.client.Close()
	a.mu.Lock()
	delete(a.sessions, s.id)
	a.mu.Unlock()

	// 记录断开事件（含会话时长）
	if !s.connectedAt.IsZero() {
		a.addLog(ConnLog{
			ServerID: s.srv.ID, Name: s.srv.Name, Host: s.srv.Host, Port: s.srv.Port,
			Username: s.srv.Username, Protocol: "ssh", Event: "disconnect",
			Status: "success", Message: "会话时长 " + time.Since(s.connectedAt).Round(time.Second).String(),
		})
	}
}

func (a *App) emitOutput(s *SSHSession, data string) {
	s.mu.Lock()
	// 维护最近输出环形缓冲，供 Agent read_terminal 读取
	s.recentOut = append(s.recentOut, data...)
	if len(s.recentOut) > maxRecentOut {
		s.recentOut = s.recentOut[len(s.recentOut)-maxRecentOut:]
	}
	if s.closed {
		s.mu.Unlock()
		return
	}
	if !s.attached {
		if len(s.buf) < maxPendingBuf {
			s.buf = append(s.buf, data...)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	wailsRuntime.EventsEmit(a.ctx, "ssh:out:"+s.id, data)
}

func (a *App) newSSHSession(sv Server, cols, rows int) (*SSHSession, error) {
	client, err := a.dialSSH(sv)
	if err != nil {
		return nil, err
	}

	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("创建会话失败: %v", err)
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 32
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("请求终端失败: %v", err)
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	if err := sess.Shell(); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("启动 Shell 失败: %v", err)
	}

	s := &SSHSession{id: randomID(), name: sv.Name, client: client, session: sess, stdin: stdin, srv: sv, connectedAt: time.Now()}
	a.mu.Lock()
	a.sessions[s.id] = s
	a.mu.Unlock()

	// 60 秒内前端未接入的会话自动清理
	time.AfterFunc(60*time.Second, func() {
		s.mu.Lock()
		orphan := !s.attached && !s.closed
		s.mu.Unlock()
		if orphan {
			a.closeSession(s)
		}
	})

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				a.emitOutput(s, string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
		s.mu.Lock()
		s.exited = true
		attached := s.attached
		closed := s.closed
		s.mu.Unlock()
		if attached && !closed {
			wailsRuntime.EventsEmit(a.ctx, "ssh:exit:"+s.id)
		}
		// 给前端留出处理退出事件的时间后清理资源
		time.AfterFunc(2*time.Second, func() { a.closeSession(s) })
	}()

	return s, nil
}
