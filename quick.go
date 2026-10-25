package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------------- 快捷命令 / 快捷提示词（本地 JSON 存储） ----------------

type QuickCommand struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Command string `json:"command"`
}

type QuickPrompt struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

type quickStore[T any] struct {
	mu    sync.RWMutex
	path  string
	Items []T `json:"items"`
}

func newQuickStore[T any](path string) *quickStore[T] {
	s := &quickStore[T]{path: path, Items: []T{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, s)
	}
	return s
}

func (s *quickStore[T]) saveLocked() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (s *quickStore[T]) list() []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]T, len(s.Items))
	copy(out, s.Items)
	return out
}

// upsert 按 getID 匹配；id 为空则分配新 id 追加
func (s *quickStore[T]) upsert(item T, getID func(T) string, setID func(*T, string)) (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := getID(item)
	if id == "" {
		setID(&item, randomID())
		s.Items = append(s.Items, item)
		return item, s.saveLocked()
	}
	for i, old := range s.Items {
		if getID(old) == id {
			s.Items[i] = item
			return item, s.saveLocked()
		}
	}
	setID(&item, randomID())
	s.Items = append(s.Items, item)
	return item, s.saveLocked()
}

func (s *quickStore[T]) del(getID func(T) string, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.Items {
		if getID(old) == id {
			s.Items = append(s.Items[:i], s.Items[i+1:]...)
			return s.saveLocked()
		}
	}
	return errors.New("条目不存在")
}

// replaceAll 全量替换条目（云同步恢复时使用）
func (s *quickStore[T]) replaceAll(items []T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Items = items
	return s.saveLocked()
}

var (
	quickCmdStore    = newQuickStore[QuickCommand](dataFile("commands.json"))
	quickPromptStore = newQuickStore[QuickPrompt](dataFile("prompts.json"))
)

func cmdID(c QuickCommand) string { return c.ID }
func cmdSetID(c *QuickCommand, id string) {
	c.ID = id
}
func promptID(p QuickPrompt) string { return p.ID }
func promptSetID(p *QuickPrompt, id string) {
	p.ID = id
}

func (a *App) GetQuickCommands() []QuickCommand { return quickCmdStore.list() }

func (a *App) SaveQuickCommand(c QuickCommand) (QuickCommand, error) {
	c.Title = strings.TrimSpace(c.Title)
	c.Command = strings.TrimRight(c.Command, "\n")
	if c.Title == "" || strings.TrimSpace(c.Command) == "" {
		return QuickCommand{}, errors.New("标题与命令不能为空")
	}
	return quickCmdStore.upsert(c, cmdID, cmdSetID)
}

func (a *App) DeleteQuickCommand(id string) error { return quickCmdStore.del(cmdID, id) }

func (a *App) GetQuickPrompts() []QuickPrompt { return quickPromptStore.list() }

func (a *App) SaveQuickPrompt(p QuickPrompt) (QuickPrompt, error) {
	p.Title = strings.TrimSpace(p.Title)
	p.Content = strings.TrimSpace(p.Content)
	if p.Title == "" || p.Content == "" {
		return QuickPrompt{}, errors.New("标题与内容不能为空")
	}
	return quickPromptStore.upsert(p, promptID, promptSetID)
}

func (a *App) DeleteQuickPrompt(id string) error { return quickPromptStore.del(promptID, id) }

// ---------------- AI 辅助批量导入主机 ----------------

// ParseServersFromText 让 LLM 把粘贴的文本解析为主机列表（JSON），返回待确认的 Server 数组
func (a *App) ParseServersFromText(text string) ([]Server, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("请粘贴要解析的主机信息文本")
	}
	if len(text) > 32*1024 {
		text = text[:32*1024]
	}
	cfg := (&aiConfigStore{path: a.aiConfigPath()}).load()
	if cfg.BaseURL == "" || cfg.Model == "" {
		return nil, errors.New("请先在 AI 设置中配置接口地址与模型")
	}

	reqMsgs := []AIMessage{
		{Role: "system", Content: "你是数据解析助手。把用户粘贴的文本（可能包含表格、列表、注释）解析为 SSH/服务器主机列表，只输出 JSON 数组，每项格式：{\"name\":\"主机名\",\"host\":\"IP或域名\",\"port\":22,\"username\":\"root\",\"password\":\"\",\"category\":\"分组\"}。规则：port 为数字，缺省 22；username 缺省 root；无法确定的字段用空字符串；文本中有明显分组/环境信息（如 生产/测试）时填入 category，否则留空。只输出 JSON，不要任何解释。"},
		{Role: "user", Content: text},
	}
	body := map[string]interface{}{
		"model":    cfg.Model,
		"messages": reqMsgs,
		"stream":   false,
	}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	client := apiHTTPClient(2 * time.Minute)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接 AI 接口：%v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %v", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("接口返回 %d：%s", resp.StatusCode, truncateStr(strings.TrimSpace(string(data)), 300))
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &cr); err != nil || len(cr.Choices) == 0 {
		return nil, errors.New("AI 未返回有效内容")
	}
	content := strings.TrimSpace(cr.Choices[0].Message.Content)

	// 提取 JSON 数组（模型可能包裹 ```json 代码块）
	start := strings.Index(content, "[")
	end := strings.LastIndex(content, "]")
	if start < 0 || end <= start {
		return nil, errors.New("AI 返回内容中未找到主机列表 JSON，请调整文本后重试")
	}
	var servers []Server
	if err := json.Unmarshal([]byte(content[start:end+1]), &servers); err != nil {
		return nil, fmt.Errorf("解析 AI 返回的 JSON 失败: %v", err)
	}
	if len(servers) == 0 {
		return nil, errors.New("未解析出任何主机")
	}
	out := make([]Server, 0, len(servers))
	for i := range servers {
		sv := servers[i]
		sv.Name = strings.TrimSpace(sv.Name)
		sv.Host = strings.TrimSpace(sv.Host)
		if sv.Name == "" {
			sv.Name = sv.Host
		}
		if sv.Host == "" {
			continue
		}
		sv.Type = "ssh"
		normalizeServer(&sv)
		out = append(out, sv)
	}
	if len(out) == 0 {
		return nil, errors.New("解析结果中没有有效主机（缺少主机地址）")
	}
	return out, nil
}
