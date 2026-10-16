package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// ---------------- 连接日志 ----------------

// maxConnLogs 日志最多保留条数，超出后裁剪最旧记录
const maxConnLogs = 500

// ConnLog 一条连接日志
// Event: connect | disconnect；Status: success | fail；Protocol: ssh | rdp
type ConnLog struct {
	ID       string `json:"id"`
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Protocol string `json:"protocol"`
	Event    string `json:"event"`
	Status   string `json:"status"`
	Message  string `json:"message"`
	Time     int64  `json:"time"` // Unix 秒
}

// logStore 连接日志的 JSON 持久化（与 serverStore 同款模式）
type logStore struct {
	mu   sync.RWMutex
	path string
	Logs []ConnLog `json:"logs"`
}

func newLogStore(path string) *logStore {
	s := &logStore{path: path, Logs: []ConnLog{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, s)
	}
	return s
}

func (s *logStore) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

// append 追加一条日志（最新在前），超过上限时裁剪最旧记录
func (s *logStore) append(l ConnLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.ID == "" {
		l.ID = randomID()
	}
	if l.Time == 0 {
		l.Time = time.Now().Unix()
	}
	s.Logs = append([]ConnLog{l}, s.Logs...)
	if len(s.Logs) > maxConnLogs {
		s.Logs = s.Logs[:maxConnLogs]
	}
	return s.save()
}

func (s *logStore) list() []ConnLog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ConnLog, len(s.Logs))
	copy(out, s.Logs)
	return out
}

func (s *logStore) clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Logs = []ConnLog{}
	return s.save()
}

// addLog 安全写入一条连接日志（未初始化或测试环境下静默跳过）
func (a *App) addLog(l ConnLog) {
	if a.logs == nil {
		return
	}
	_ = a.logs.append(l)
}

// GetConnLogs 返回全部连接日志（最新在前），供前端展示
func (a *App) GetConnLogs() []ConnLog {
	if a.logs == nil {
		return []ConnLog{}
	}
	return a.logs.list()
}

// ClearConnLogs 清空全部连接日志
func (a *App) ClearConnLogs() error {
	if a.logs == nil {
		return nil
	}
	return a.logs.clear()
}
