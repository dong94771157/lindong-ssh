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
	if c.BaseURL == "" || c.Model == "" || c.APIKey == "" {
		return errors.New("接口地址、模型名称和 API Key 均不能为空")
	}
	return (&aiConfigStore{path: a.aiConfigPath()}).save(c)
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
	if cfg.BaseURL == "" || cfg.Model == "" || cfg.APIKey == "" {
		return errors.New("请先在 AI 设置中配置接口地址、模型与 API Key")
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
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
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
