package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ---------------- 零知识云同步 ----------------
//
// 密钥体系（客户端零知识）：
//   - aiss_* 鉴权密钥：云端账号身份，用于请求鉴权。只在生成时完整显示一次，请立即复制保存。
//   - enc_*  本地加密密钥：AES-256-GCM 加密密钥，由客户端生成，永不上传、丢失无法找回。
//             云端只保存密文，服务端无法读取用户数据。
//
// 同步范围：主机列表、快捷提示词、快捷命令（不含 AI 对话记录、API Key）。
// 云端账号连续 365 天未使用将被自动清理。

// SyncEndpoint 云端同步服务地址（写死，用户无需自行填写）
const SyncEndpoint = "https://cloud.ldnet.sryze.cc"

type SyncConfig struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"` // 云端 KV 服务地址，如 https://sync.example.com（自建见 worker/sync-worker.js）
	AuthKey  string `json:"authKey"`  // aiss_* 鉴权密钥
	EncKey   string `json:"encKey"`   // enc_* 本地加密密钥（32 字节 hex）
	LastSync int64  `json:"lastSync"` // 最后成功同步的远端快照时间戳
	LastHash string `json:"lastHash"` // 上次同步的本地数据哈希（用于变更检测）
}

type syncStore struct {
	mu   sync.RWMutex
	path string
	cfg  SyncConfig
}

func newSyncStore(path string) *syncStore {
	s := &syncStore{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.cfg)
	}
	s.cfg.Endpoint = SyncEndpoint // 始终使用内置云端地址
	return s
}

var syncSt = newSyncStore(dataFile("sync.json"))

func (s *syncStore) save() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600) // 含密钥，仅本人可读写
}

// maskSyncKey 密钥脱敏显示（保留前缀与首尾各 4 位）
func maskSyncKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 12 {
		return k + "********"
	}
	return k[:8] + "********" + k[len(k)-4:]
}

// GetSyncConfig 返回脱敏后的配置（密钥不完整回传，符合「只显示一次」约定）
func (a *App) GetSyncConfig() SyncConfig {
	syncSt.mu.RLock()
	defer syncSt.mu.RUnlock()
	c := syncSt.cfg
	c.Endpoint = SyncEndpoint // 云端地址已内置，无需用户填写
	c.AuthKey = maskSyncKey(c.AuthKey)
	c.EncKey = maskSyncKey(c.EncKey)
	return c
}

// GenerateSyncKeys 生成新的 aiss_*/enc_* 密钥对并保存。
// 返回的配置包含完整密钥——仅此一次完整显示，请用户立即复制保存。
// 注意：重新生成相当于换一个全新的云端账号，旧密钥对应的云端数据将无法再访问。
func (a *App) GenerateSyncKeys() (SyncConfig, error) {
	ab := make([]byte, 16)
	eb := make([]byte, 32)
	if _, err := rand.Read(ab); err != nil {
		return SyncConfig{}, err
	}
	if _, err := rand.Read(eb); err != nil {
		return SyncConfig{}, err
	}
	syncSt.mu.Lock()
	defer syncSt.mu.Unlock()
	syncSt.cfg.AuthKey = "aiss_" + hex.EncodeToString(ab)
	syncSt.cfg.EncKey = "enc_" + hex.EncodeToString(eb)
	syncSt.cfg.LastSync = 0
	syncSt.cfg.LastHash = ""
	if err := syncSt.save(); err != nil {
		return SyncConfig{}, err
	}
	return syncSt.cfg, nil
}

// SaveSyncConfig 保存开关；AuthKey/EncKey 传空或脱敏值时保留原密钥不变
// （允许用户粘贴完整密钥以绑定已有云端账号，例如换机恢复场景）
// 云端地址已内置写死，无需用户填写
func (a *App) SaveSyncConfig(c SyncConfig) error {
	syncSt.mu.Lock()
	defer syncSt.mu.Unlock()
	c.Endpoint = SyncEndpoint // 云端地址已内置，强制覆盖
	if strings.Contains(c.AuthKey, "*") || strings.TrimSpace(c.AuthKey) == "" {
		c.AuthKey = syncSt.cfg.AuthKey
	} else if !strings.HasPrefix(c.AuthKey, "aiss_") {
		return errors.New("鉴权密钥须以 aiss_ 开头")
	}
	if strings.Contains(c.EncKey, "*") || strings.TrimSpace(c.EncKey) == "" {
		c.EncKey = syncSt.cfg.EncKey
	} else if _, err := syncEncKeyBytes(c.EncKey); err != nil {
		return err
	}
	c.LastSync = syncSt.cfg.LastSync
	c.LastHash = syncSt.cfg.LastHash
	syncSt.cfg = c
	return syncSt.save()
}

// syncEncKeyBytes 解析 enc_* 密钥为 32 字节 AES-256 密钥
func syncEncKeyBytes(encKey string) ([]byte, error) {
	h := strings.TrimPrefix(strings.TrimSpace(encKey), "enc_")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 32 {
		return nil, errors.New("加密密钥格式不正确（应为 enc_ 开头的 64 位十六进制）")
	}
	return b, nil
}

// syncSnapshot 同步快照（加密前的明文结构）
type syncSnapshot struct {
	TS       int64          `json:"ts"`
	Servers  []Server       `json:"servers"`
	Prompts  []QuickPrompt  `json:"prompts"`
	Commands []QuickCommand `json:"commands"`
}

func (a *App) localSnapshot() *syncSnapshot {
	return &syncSnapshot{
		Servers:  a.store.list(),
		Prompts:  quickPromptStore.list(),
		Commands: quickCmdStore.list(),
	}
}

// snapshotHash 计算数据内容哈希（不含时间戳），用于变更检测
func snapshotHash(s *syncSnapshot) string {
	data, _ := json.Marshal(struct {
		Servers  []Server       `json:"servers"`
		Prompts  []QuickPrompt  `json:"prompts"`
		Commands []QuickCommand `json:"commands"`
	}{s.Servers, s.Prompts, s.Commands})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func syncEncrypt(plain []byte, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func syncDecrypt(b64 string, key []byte) (*syncSnapshot, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("云端数据解码失败")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("云端数据损坏")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return nil, errors.New("解密失败：加密密钥（enc_*）与云端数据不匹配")
	}
	var snap syncSnapshot
	if err := json.Unmarshal(plain, &snap); err != nil {
		return nil, errors.New("云端数据解析失败")
	}
	return &snap, nil
}

// syncRemote 云端返回的密文包裹
type syncRemote struct {
	TS   int64  `json:"ts"`
	Data string `json:"data"`
}

// syncDownload 从云端拉取快照；远端无数据时返回 (nil, nil)
func syncDownload(cfg SyncConfig) (*syncSnapshot, error) {
	req, err := http.NewRequest("GET", cfg.Endpoint+"/v1/sync/snapshot", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthKey)
	resp, err := apiHTTPClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接云端：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, errors.New("云端鉴权失败：请检查 aiss_ 鉴权密钥是否正确")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("云端返回 %d：%s", resp.StatusCode, truncateStr(strings.TrimSpace(string(data)), 200))
	}
	var r syncRemote
	if err := json.Unmarshal(data, &r); err != nil || r.Data == "" {
		return nil, errors.New("云端响应格式不正确")
	}
	key, err := syncEncKeyBytes(cfg.EncKey)
	if err != nil {
		return nil, err
	}
	snap, err := syncDecrypt(r.Data, key)
	if err != nil {
		return nil, err
	}
	if r.TS > 0 {
		snap.TS = r.TS
	}
	return snap, nil
}

// syncUpload 将本地快照加密后上传到云端
func syncUpload(cfg SyncConfig, snap *syncSnapshot) error {
	key, err := syncEncKeyBytes(cfg.EncKey)
	if err != nil {
		return err
	}
	snap.TS = time.Now().Unix()
	plain, _ := json.Marshal(snap)
	enc, err := syncEncrypt(plain, key)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(syncRemote{TS: snap.TS, Data: enc})
	req, err := http.NewRequest("PUT", cfg.Endpoint+"/v1/sync/snapshot", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiHTTPClient(30 * time.Second).Do(req)
	if err != nil {
		return fmt.Errorf("无法连接云端：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return errors.New("云端鉴权失败：请检查 aiss_ 鉴权密钥是否正确")
	}
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return fmt.Errorf("云端返回 %d：%s", resp.StatusCode, truncateStr(strings.TrimSpace(string(data)), 200))
	}
	syncSt.mu.Lock()
	syncSt.cfg.LastSync = snap.TS
	syncSt.cfg.LastHash = snapshotHash(snap)
	_ = syncSt.save()
	syncSt.mu.Unlock()
	return nil
}

// applyRemote 用远端快照覆盖本地数据，并通知前端刷新
func (a *App) applyRemote(snap *syncSnapshot) (string, error) {
	for i := range snap.Servers {
		normalizeServer(&snap.Servers[i])
	}
	if err := a.store.replaceAll(snap.Servers); err != nil {
		return "", err
	}
	if err := quickPromptStore.replaceAll(snap.Prompts); err != nil {
		return "", err
	}
	if err := quickCmdStore.replaceAll(snap.Commands); err != nil {
		return "", err
	}
	syncSt.mu.Lock()
	syncSt.cfg.LastSync = snap.TS
	syncSt.cfg.LastHash = snapshotHash(snap)
	_ = syncSt.save()
	syncSt.mu.Unlock()
	if a.ctx != nil {
		wailsRuntime.EventsEmit(a.ctx, "sync:applied")
	}
	return fmt.Sprintf("已从云端恢复：%d 台主机、%d 条快捷提示词、%d 条快捷命令",
		len(snap.Servers), len(snap.Prompts), len(snap.Commands)), nil
}

// syncOnce 执行一轮同步：远端更新则拉取覆盖，本地变更则加密上传
func (a *App) syncOnce() (string, error) {
	syncSt.mu.RLock()
	cfg := syncSt.cfg
	syncSt.mu.RUnlock()
	if cfg.AuthKey == "" || cfg.EncKey == "" {
		return "", errors.New("请先生成或填写 aiss_/enc_ 密钥")
	}
	if cfg.Endpoint == "" {
		return "", errors.New("请先填写云端服务地址")
	}
	local := a.localSnapshot()
	localHash := snapshotHash(local)

	remote, err := syncDownload(cfg)
	if err != nil {
		return "", err
	}
	if remote != nil && remote.TS > cfg.LastSync && snapshotHash(remote) != localHash {
		// 远端有更新的数据 → 覆盖本地
		return a.applyRemote(remote)
	}
	if localHash == cfg.LastHash && cfg.LastSync > 0 {
		return "数据已是最新，无需同步", nil
	}
	// 本地有变更（或首次备份）→ 上传
	if len(local.Servers) == 0 && len(local.Prompts) == 0 && len(local.Commands) == 0 && remote != nil {
		return "本地数据为空，为避免覆盖云端数据已取消上传（可使用「从云端恢复」）", nil
	}
	if err := syncUpload(cfg, local); err != nil {
		return "", err
	}
	return fmt.Sprintf("已加密上传到云端：%d 台主机、%d 条快捷提示词、%d 条快捷命令",
		len(local.Servers), len(local.Prompts), len(local.Commands)), nil
}

// SyncNow 手动触发一次同步，返回结果描述
func (a *App) SyncNow() (string, error) {
	syncSt.mu.RLock()
	en := syncSt.cfg.Enabled
	syncSt.mu.RUnlock()
	if !en {
		return "", errors.New("请先开启云同步并保存设置")
	}
	return a.syncOnce()
}

// SyncRestore 强制从云端拉取并覆盖本地数据（换机恢复用）
func (a *App) SyncRestore() (string, error) {
	syncSt.mu.RLock()
	cfg := syncSt.cfg
	syncSt.mu.RUnlock()
	if cfg.AuthKey == "" || cfg.EncKey == "" || cfg.Endpoint == "" {
		return "", errors.New("请先完善云端地址与 aiss_/enc_ 密钥")
	}
	remote, err := syncDownload(cfg)
	if err != nil {
		return "", err
	}
	if remote == nil {
		return "", errors.New("云端暂无备份数据")
	}
	return a.applyRemote(remote)
}

// syncLoop 后台准实时同步：每 30 秒检查一次本地变更与远端更新
func (a *App) syncLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		syncSt.mu.RLock()
		en := syncSt.cfg.Enabled && syncSt.cfg.AuthKey != "" && syncSt.cfg.EncKey != "" && syncSt.cfg.Endpoint != ""
		syncSt.mu.RUnlock()
		if !en || a.ctx == nil {
			continue
		}
		if msg, err := a.syncOnce(); err == nil {
			wailsRuntime.EventsEmit(a.ctx, "sync:done", msg)
		} else {
			wailsRuntime.EventsEmit(a.ctx, "sync:error", err.Error())
		}
	}
}
