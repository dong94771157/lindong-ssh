package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ---------------- AI 配置（OpenAI 兼容接口） ----------------

type AIConfig struct {
	BaseURL string `json:"baseUrl"` // 形如 https://api.openai.com/v1
	APIKey  string `json:"apiKey"`
	Model   string `json:"model"`
}

type aiConfigStore struct {
	path string
}

func (s *aiConfigStore) load() AIConfig {
	var c AIConfig
	if data, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

func (s *aiConfigStore) save(c AIConfig) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (a *App) aiConfigPath() string {
	return dataFile("ai.json")
}

func (a *App) GetAIConfig() AIConfig {
	return (&aiConfigStore{path: a.aiConfigPath()}).load()
}

func (a *App) SaveAIConfig(c AIConfig) error {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.Model = strings.TrimSpace(c.Model)
	c.APIKey = strings.TrimSpace(c.APIKey)
	if c.BaseURL == "" || c.Model == "" {
		return errors.New("接口地址与模型名称不能为空")
	}
	return (&aiConfigStore{path: a.aiConfigPath()}).save(c)
}

// ---------------- 上下文摘要压缩 ----------------

// SummarizeHistory 将较早的对话历史压缩为一段摘要（非流式调用）。
// 前端在估算 token 超过软上限时调用，返回的摘要替换旧历史以控制上下文长度。
func (a *App) SummarizeHistory(messages []AIMessage) (string, error) {
	cfg := (&aiConfigStore{path: a.aiConfigPath()}).load()
	if cfg.BaseURL == "" || cfg.Model == "" {
		return "", errors.New("请先在 AI 设置中配置接口地址与模型")
	}
	if len(messages) == 0 {
		return "", errors.New("没有需要压缩的历史")
	}

	var sb strings.Builder
	for _, m := range messages {
		role := "用户"
		if m.Role == "assistant" {
			role = "助手"
		}
		sb.WriteString(role + "：")
		sb.WriteString(m.Content)
		sb.WriteString("\n\n")
	}
	dialog := sb.String()
	if len(dialog) > 120*1024 {
		dialog = dialog[:120*1024] // 防止摘要请求本身超限
	}

	reqMsgs := []AIMessage{
		{Role: "system", Content: "你是对话摘要助手。将下面的运维对话压缩为简洁的中文摘要（500 字以内），必须保留：执行过的关键命令、发现的问题、已完成的修改、未解决的待办、重要的主机/路径/配置信息。只输出摘要正文，不要额外说明。"},
		{Role: "user", Content: dialog},
	}
	body := map[string]interface{}{
		"model":    cfg.Model,
		"messages": reqMsgs,
		"stream":   false,
	}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("无法连接 AI 接口：%v", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %v", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("接口返回 %d：%s", resp.StatusCode, truncateStr(strings.TrimSpace(string(data)), 300))
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", fmt.Errorf("解析响应失败: %v", err)
	}
	if len(cr.Choices) == 0 || strings.TrimSpace(cr.Choices[0].Message.Content) == "" {
		return "", errors.New("摘要接口未返回内容")
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), nil
}

// ---------------- AI 对话（SSE 流式） ----------------

type AIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatChunkChoice struct {
	Index int `json:"index"`
	Delta struct {
		Content string `json:"content"`
	} `json:"delta"`
}

type chatChunk struct {
	Choices []chatChunkChoice `json:"choices"`
}

// AIChat 发起流式对话，结果通过事件推送：
//
//	ai:delta:<chatId>  增量文本
//	ai:done:<chatId>   完成
//	ai:error:<chatId>  错误信息
func (a *App) AIChat(chatID string, messages []AIMessage) error {
	if chatID == "" {
		return errors.New("缺少会话ID")
	}
	cfg := (&aiConfigStore{path: a.aiConfigPath()}).load()
	if cfg.BaseURL == "" || cfg.Model == "" {
		return errors.New("请先在 AI 设置中配置接口地址与模型")
	}
	if len(messages) == 0 {
		return errors.New("消息不能为空")
	}

	body := map[string]interface{}{
		"model":    cfg.Model,
		"messages": messages,
		"stream":   true,
	}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		wailsRuntime.EventsEmit(a.ctx, "ai:error:"+chatID, "无法连接 AI 接口："+err.Error())
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		wailsRuntime.EventsEmit(a.ctx, "ai:error:"+chatID, fmt.Sprintf("接口返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(b))))
		return nil
	}

	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				wailsRuntime.EventsEmit(a.ctx, "ai:done:"+chatID)
				return
			}
			var ch chatChunk
			if err := json.Unmarshal([]byte(payload), &ch); err != nil || len(ch.Choices) == 0 {
				continue
			}
			if delta := ch.Choices[0].Delta.Content; delta != "" {
				wailsRuntime.EventsEmit(a.ctx, "ai:delta:"+chatID, delta)
			}
		}
		if err := scanner.Err(); err != nil {
			wailsRuntime.EventsEmit(a.ctx, "ai:error:"+chatID, "读取响应中断："+err.Error())
			return
		}
		wailsRuntime.EventsEmit(a.ctx, "ai:done:"+chatID)
	}()
	return nil
}
