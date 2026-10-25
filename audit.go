package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// ---------------- 安全配置（只读文件系统等） ----------------

type SecurityConfig struct {
	ReadOnlyFS bool `json:"readOnlyFs"` // 开启后 Agent 的所有远程写操作被拦截
}

type securityStore struct {
	mu   sync.RWMutex
	path string
	cfg  SecurityConfig
}

func newSecurityStore(path string) *securityStore {
	s := &securityStore{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.cfg)
	}
	return s
}

var secStore = newSecurityStore(dataFile("security.json"))

func (a *App) GetSecurityConfig() SecurityConfig {
	secStore.mu.RLock()
	defer secStore.mu.RUnlock()
	return secStore.cfg
}

func (a *App) SaveSecurityConfig(c SecurityConfig) error {
	secStore.mu.Lock()
	defer secStore.mu.Unlock()
	secStore.cfg = c
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(secStore.path, data, 0644)
}

func securityReadOnly() bool {
	secStore.mu.RLock()
	defer secStore.mu.RUnlock()
	return secStore.cfg.ReadOnlyFS
}

// fsWriteTools 只读模式下被拦截的远程写工具
var fsWriteTools = map[string]bool{
	"sftp_write":   true,
	"sftp_delete":  true,
	"sftp_rename":  true,
	"sftp_mkdir":   true,
	"sftp_upload":  true,
	"write_terminal": true,
}

// ---------------- 审计留痕 ----------------

type AuditEntry struct {
	Time      string `json:"time"`
	AgentID   string `json:"agentId"`
	SessionID string `json:"sessionId"`
	Tool      string `json:"tool"`
	Args      string `json:"args"`
	Status    string `json:"status"` // done / error / blocked / denied
	Result    string `json:"result"`
}

type auditStore struct {
	mu      sync.Mutex
	path    string
	Entries []AuditEntry `json:"entries"`
}

var audit = &auditStore{path: dataFile("audit.json"), Entries: []AuditEntry{}}

func init() {
	if data, err := os.ReadFile(audit.path); err == nil {
		_ = json.Unmarshal(data, &audit.Entries)
	}
}

const auditMaxEntries = 2000

// appendAudit 记录一次 Agent 工具调用
func appendAudit(agentID, sessionID, tool, args, status, result string) {
	audit.mu.Lock()
	defer audit.mu.Unlock()
	audit.Entries = append(audit.Entries, AuditEntry{
		Time:      time.Now().Format("2006-01-02 15:04:05"),
		AgentID:   agentID,
		SessionID: sessionID,
		Tool:      tool,
		Args:      truncateStr(args, 300),
		Status:    status,
		Result:    truncateStr(result, 300),
	})
	if len(audit.Entries) > auditMaxEntries {
		audit.Entries = audit.Entries[len(audit.Entries)-auditMaxEntries:]
	}
	if data, err := json.MarshalIndent(audit.Entries, "", "  "); err == nil {
		_ = os.WriteFile(audit.path, data, 0644)
	}
}

func (a *App) GetAuditLogs() []AuditEntry {
	audit.mu.Lock()
	defer audit.mu.Unlock()
	out := make([]AuditEntry, len(audit.Entries))
	copy(out, audit.Entries)
	return out
}

func (a *App) ClearAuditLogs() error {
	audit.mu.Lock()
	defer audit.mu.Unlock()
	audit.Entries = []AuditEntry{}
	return os.WriteFile(audit.path, []byte("[]"), 0644)
}
