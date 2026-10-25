package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ---------------- Agent 模式（OpenAI 兼容 function calling） ----------------

const (
	agentMaxRounds    = 20 // 主循环最大轮次
	agentMaxToolCalls = 60 // 单次任务全局工具调用上限
	agentMaxDepth     = 2  // 子代理最大嵌套深度
)

const agentSystemPrompt = `你是「琳东SSH管理软件」内置的运维 Agent，可以通过工具直接操作远端 Linux 主机完成任务。

工作原则：
1. 先探查后操作：优先使用只读工具（sys_info、grep、file_tail、sftp_read 等）了解环境，再执行修改类操作。
2. 危险操作前必须先调用 ask_user 向用户说明风险并获得批准；系统也会对高危命令自动拦截并要求用户确认。
3. 命令执行失败时分析错误原因，调整策略重试，不要盲目重复相同命令。
4. 复杂任务可以用 delegate_subtask 拆分给子代理处理（子代理共享当前主机连接）。
5. 修改配置文件优先用 sftp_write（会自动备份原文件），修改后用 validate_config 校验语法。
6. 需要交互的命令（如 passwd、top、less）用 write_terminal 写入交互终端，再用 read_terminal 观察输出。
7. 回答使用中文，简洁直接，说明做了什么、结果如何。
8. 任务完成后给出总结，不要无意义地继续调用工具。`

// agentMessage 支持 tool_calls 的对话消息
type agentMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON 字符串
	} `json:"function"`
}

type agentChatRequest struct {
	Model      string                   `json:"model"`
	Messages   []agentMessage           `json:"messages"`
	Tools      []map[string]interface{} `json:"tools,omitempty"`
	ToolChoice string                   `json:"tool_choice,omitempty"`
	Stream     bool                     `json:"stream"`
}

type agentChatResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------- 工具调用计数（按 agentID，子代理共享） ----------------

func (a *App) agentCallReset(agentID string) {
	a.agentMu.Lock()
	defer a.agentMu.Unlock()
	a.agentCalls[agentID] = new(int)
}

// agentCallIncr 递增调用计数，超过上限返回 false
func (a *App) agentCallIncr(agentID string) (int, bool) {
	a.agentMu.Lock()
	defer a.agentMu.Unlock()
	p, ok := a.agentCalls[agentID]
	if !ok {
		p = new(int)
		a.agentCalls[agentID] = p
	}
	*p++
	return *p, *p <= agentMaxToolCalls
}

func (a *App) agentCallCleanup(agentID string) {
	a.agentMu.Lock()
	defer a.agentMu.Unlock()
	delete(a.agentCalls, agentID)
}

// ---------------- 用户确认（ask_user 阻塞等待） ----------------

// askUserAndWait 推送确认请求并阻塞等待前端答复，超时视为拒绝
func (a *App) askUserAndWait(agentID, question, risk string, timeout time.Duration) (askResponse, error) {
	a.agentMu.Lock()
	ch := make(chan askResponse, 1)
	a.askChans[agentID] = ch
	a.agentMu.Unlock()
	defer func() {
		a.agentMu.Lock()
		delete(a.askChans, agentID)
		a.agentMu.Unlock()
	}()

	wailsRuntime.EventsEmit(a.ctx, "agent:ask:"+agentID, map[string]string{
		"question": question,
		"risk":     risk,
	})
	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		return askResponse{Approved: false, Note: "等待用户确认超时（120 秒）"}, nil
	}
}

// RespondAsk 前端调用：答复 Agent 的确认请求
func (a *App) RespondAsk(agentID string, approved bool, note string) error {
	a.agentMu.Lock()
	ch, ok := a.askChans[agentID]
	a.agentMu.Unlock()
	if !ok {
		return errors.New("确认请求不存在或已超时")
	}
	select {
	case ch <- askResponse{Approved: approved, Note: strings.TrimSpace(note)}:
	default:
	}
	return nil
}

// ---------------- Agent 主循环 ----------------

// AgentChat 启动 Agent 模式任务（异步），结果通过事件推送：
//
//	agent:delta:<agentID>  助手中间文本
//	agent:final:<agentID>  助手最终回答
//	agent:tool:<agentID>   工具调用状态（running/done/error）
//	agent:ask:<agentID>    请求用户确认
//	agent:done:<agentID>   任务结束
//	agent:error:<agentID>  错误信息
func (a *App) AgentChat(agentID, sessionID string, history []AIMessage, userText string, depth int) error {
	if agentID == "" {
		return errors.New("缺少 Agent 会话ID")
	}
	if strings.TrimSpace(userText) == "" {
		return errors.New("任务内容不能为空")
	}
	if a.getSession(sessionID) == nil {
		return errors.New("终端会话不存在，请先连接主机并激活对应终端标签")
	}
	cfg := (&aiConfigStore{path: a.aiConfigPath()}).load()
	if cfg.BaseURL == "" || cfg.Model == "" {
		return errors.New("请先在 AI 设置中配置接口地址与模型")
	}
	a.agentCallReset(agentID)
	go func() {
		if depth < 0 || depth > agentMaxDepth {
			depth = 0
		}
		_, err := a.runAgentLoop(agentID, sessionID, history, userText, depth, false)
		if err != nil {
			wailsRuntime.EventsEmit(a.ctx, "agent:error:"+agentID, err.Error())
		}
		a.agentCallCleanup(agentID)
		wailsRuntime.EventsEmit(a.ctx, "agent:done:"+agentID)
	}()
	return nil
}

// runAgentLoop Agent 主循环；silent 为 true（子代理）时不推前端事件，返回最终文本
func (a *App) runAgentLoop(agentID, sessionID string, history []AIMessage, userText string, depth int, silent bool) (string, error) {
	cfg := (&aiConfigStore{path: a.aiConfigPath()}).load()
	if cfg.BaseURL == "" || cfg.Model == "" {
		return "", errors.New("请先在 AI 设置中配置接口地址与模型")
	}

	msgs := make([]agentMessage, 0, len(history)+2)
	msgs = append(msgs, agentMessage{Role: "system", Content: agentSystemPrompt})
	for _, m := range history {
		if m.Role == "user" || m.Role == "assistant" {
			msgs = append(msgs, agentMessage{Role: m.Role, Content: m.Content})
		}
	}
	msgs = append(msgs, agentMessage{Role: "user", Content: userText})

	defs := agentTools()
	schemas := toolSchemas(defs)
	handlers := make(map[string]func(*agentCtx, map[string]interface{}) (string, error), len(defs))
	for _, d := range defs {
		handlers[d.Name] = d.Handler
	}
	ctx := &agentCtx{app: a, agentID: agentID, sessionID: sessionID, depth: depth}

	for round := 0; round < agentMaxRounds; round++ {
		resp, err := a.callAgentAPI(cfg, msgs, schemas)
		if err != nil {
			return "", err
		}
		choice := resp.Choices[0]
		content := choice.Message.Content
		calls := choice.Message.ToolCalls

		if len(calls) == 0 {
			// 模型返回纯文本，任务结束
			if strings.TrimSpace(content) == "" {
				content = "（模型未返回内容）"
			}
			if !silent {
				wailsRuntime.EventsEmit(a.ctx, "agent:final:"+agentID, content)
			}
			return content, nil
		}

		// 有工具调用：中间文本先推给前端
		if strings.TrimSpace(content) != "" && !silent {
			wailsRuntime.EventsEmit(a.ctx, "agent:delta:"+agentID, content)
		}
		msgs = append(msgs, agentMessage{Role: "assistant", Content: content, ToolCalls: calls})

		for _, call := range calls {
			result := a.executeTool(ctx, handlers, call, silent)
			msgs = append(msgs, agentMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: call.ID,
			})
		}
	}
	return "", fmt.Errorf("已达最大轮次上限（%d 轮），任务未完成", agentMaxRounds)
}

// executeTool 执行单个工具调用，推送状态事件，返回给模型的结果文本
func (a *App) executeTool(ctx *agentCtx, handlers map[string]func(*agentCtx, map[string]interface{}) (string, error), call toolCall, silent bool) string {
	name := call.Function.Name
	rawArgs := call.Function.Arguments

	count, ok := a.agentCallIncr(ctx.agentID)
	if !ok {
		return fmt.Sprintf("工具调用次数已达全局上限（%d 次），请停止调用工具并总结当前进展", agentMaxToolCalls)
	}

	emit := func(status, result string) {
		if silent {
			return
		}
		wailsRuntime.EventsEmit(a.ctx, "agent:tool:"+ctx.agentID, map[string]interface{}{
			"id":     call.ID,
			"name":   name,
			"args":   rawArgs,
			"status": status,
			"result": result,
			"seq":    count,
		})
	}
	emit("running", "")

	handler, exists := handlers[name]
	if !exists {
		emit("error", "未知工具")
		return "错误：未知工具 " + name
	}

	var args map[string]interface{}
	if rawArgs != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			emit("error", "参数解析失败")
			return "错误：工具参数不是合法 JSON：" + err.Error()
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}

	// 只读文件系统模式：拦截远程写工具
	if fsWriteTools[name] && securityReadOnly() {
		msg := "只读文件系统模式已开启，写操作 " + name + " 已被拦截"
		emit("error", msg)
		appendAudit(ctx.agentID, ctx.sessionID, name, rawArgs, "blocked", msg)
		return msg + "。如需执行，请让用户在 AI 设置中关闭「只读文件系统」"
	}

	// 高危操作拦截：命令类工具过风险评估，危险工具强制确认
	if target, needCheck := toolRiskTarget(name, args); needCheck {
		high, reason := assessRisk(target)
		if alwaysConfirmTools[name] {
			high = true
			if reason == "" {
				reason = "该操作会修改系统状态或删除数据"
			}
		}
		if high {
			emit("running", "等待用户确认…")
			resp, err := a.askUserAndWait(ctx.agentID,
				fmt.Sprintf("Agent 请求执行高危操作【%s】\n内容：%s\n风险：%s", name, truncateStr(target, 500), reason),
				"high", 120*time.Second)
			if err != nil || !resp.Approved {
				note := "用户拒绝了该高危操作"
				if err != nil {
					note = "确认请求失败：" + err.Error()
				} else if resp.Note != "" {
					note += "，补充说明：" + resp.Note
				}
				emit("done", note)
				appendAudit(ctx.agentID, ctx.sessionID, name, rawArgs, "denied", note)
				return note + "，请不要重试相同操作，可改用更安全的替代方案或向用户说明"
			}
			emit("running", "用户已批准，继续执行")
		}
	}

	out, err := handler(ctx, args)
	if err != nil {
		emit("error", err.Error())
		appendAudit(ctx.agentID, ctx.sessionID, name, rawArgs, "error", err.Error())
		return "工具执行失败：" + err.Error()
	}
	summary := out
	if len(summary) > 500 {
		summary = summary[:500] + "…"
	}
	emit("done", summary)
	appendAudit(ctx.agentID, ctx.sessionID, name, rawArgs, "done", summary)
	return out
}

// callAgentAPI 非流式调用 chat/completions（带 tools）
func (a *App) callAgentAPI(cfg AIConfig, msgs []agentMessage, tools []map[string]interface{}) (*agentChatResponse, error) {
	body := agentChatRequest{
		Model:      cfg.Model,
		Messages:   msgs,
		Stream:     false,
		ToolChoice: "auto",
	}
	if len(tools) > 0 {
		body.Tools = tools
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

	client := apiHTTPClient(3 * time.Minute)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接 AI 接口：%v", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %v", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("接口返回 %d：%s", resp.StatusCode, truncateStr(strings.TrimSpace(string(data)), 500))
	}
	var cr agentChatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, fmt.Errorf("解析响应失败: %v", err)
	}
	if cr.Error != nil {
		return nil, errors.New("接口错误：" + cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return nil, errors.New("接口未返回任何结果")
	}
	return &cr, nil
}

// ---------------- 工具定义（OpenAI tools schema） ----------------

type toolDef struct {
	Name        string
	Description string
	Parameters  map[string]interface{} // JSON Schema
	Handler     func(*agentCtx, map[string]interface{}) (string, error)
}

func schemaObj(props map[string]interface{}, required []string) map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func intProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

// toolSchemas 转换为 OpenAI tools 数组
func toolSchemas(defs []toolDef) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(defs))
	for _, d := range defs {
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        d.Name,
				"description": d.Description,
				"parameters":  d.Parameters,
			},
		})
	}
	return out
}

// agentTools 全部 Agent 工具（35 个，分四组）
func agentTools() []toolDef {
	return []toolDef{
		// ---- 命令执行 ----
		{
			Name: "run_command", Description: "在远端主机执行单条 Shell 命令（新建非交互会话），返回标准输出与错误输出。适用于大多数探查与操作。",
			Parameters: schemaObj(map[string]interface{}{
				"command": strProp("要执行的 Shell 命令"),
				"timeout": intProp("超时秒数，默认 60，最大 300"),
			}, []string{"command"}),
			Handler: toolRunCommand,
		},
		{
			Name: "run_script", Description: "在远端主机执行多行 Shell 脚本（通过 stdin 传入），可选解释器 bash 或 sh。",
			Parameters: schemaObj(map[string]interface{}{
				"script":  strProp("多行脚本内容"),
				"shell":   strProp("解释器：bash（默认）或 sh"),
				"timeout": intProp("超时秒数，默认 60，最大 300"),
			}, []string{"script"}),
			Handler: toolRunScript,
		},
		{
			Name: "write_terminal", Description: "向当前交互式终端的 stdin 写入内容（含回车 \\n），用于需要交互的命令（如 passwd、top、vim）。写入后用 read_terminal 观察输出。",
			Parameters: schemaObj(map[string]interface{}{
				"data": strProp("要写入的内容，回车用 \\n 表示"),
			}, []string{"data"}),
			Handler: toolWriteTerminal,
		},
		{
			Name: "read_terminal", Description: "读取当前交互式终端最近的输出缓冲（最多 64KB 环形缓冲），用于观察 write_terminal 的执行结果。",
			Parameters:  schemaObj(map[string]interface{}{}, []string{}),
			Handler:     toolReadTerminal,
		},
		// ---- 文件 / SFTP ----
		{
			Name: "sftp_read", Description: "读取远程文件内容（上限 256KB，超出截断）。",
			Parameters: schemaObj(map[string]interface{}{
				"path": strProp("远程文件绝对路径"),
			}, []string{"path"}),
			Handler: toolSFTPRead,
		},
		{
			Name: "sftp_write", Description: "写入远程文件（覆盖写）。若原文件存在会自动备份为 <path>.bak-<时间戳>。",
			Parameters: schemaObj(map[string]interface{}{
				"path":    strProp("远程文件绝对路径"),
				"content": strProp("要写入的完整内容"),
			}, []string{"path", "content"}),
			Handler: toolSFTPWrite,
		},
		{
			Name: "sftp_list", Description: "列出远程目录内容（权限、大小、修改时间），最多显示 200 项。",
			Parameters: schemaObj(map[string]interface{}{
				"path": strProp("目录路径，默认当前目录"),
			}, []string{}),
			Handler: toolSFTPList,
		},
		{
			Name: "sftp_stat", Description: "获取远程文件或目录的详细信息（类型、大小、权限、修改时间）。",
			Parameters: schemaObj(map[string]interface{}{
				"path": strProp("远程路径"),
			}, []string{"path"}),
			Handler: toolSFTPStat,
		},
		{
			Name: "sftp_mkdir", Description: "递归创建远程目录（类似 mkdir -p）。",
			Parameters: schemaObj(map[string]interface{}{
				"path": strProp("要创建的目录路径"),
			}, []string{"path"}),
			Handler: toolSFTPMkdir,
		},
		{
			Name: "sftp_delete", Description: "删除远程文件或目录（目录递归删除）。高危操作，执行前需用户确认。",
			Parameters: schemaObj(map[string]interface{}{
				"path": strProp("要删除的远程路径"),
			}, []string{"path"}),
			Handler: toolSFTPDelete,
		},
		{
			Name: "sftp_rename", Description: "重命名或移动远程文件/目录。",
			Parameters: schemaObj(map[string]interface{}{
				"old_path": strProp("原路径"),
				"new_path": strProp("新路径"),
			}, []string{"old_path", "new_path"}),
			Handler: toolSFTPRename,
		},
		{
			Name: "sftp_upload", Description: "上传本地文件到远程主机（上限 100MB），自动创建目标目录。",
			Parameters: schemaObj(map[string]interface{}{
				"local_path":  strProp("本地文件路径"),
				"remote_path": strProp("远程目标完整文件路径"),
			}, []string{"local_path", "remote_path"}),
			Handler: toolSFTPUpload,
		},
		{
			Name: "sftp_download", Description: "下载远程文件到本地。",
			Parameters: schemaObj(map[string]interface{}{
				"remote_path": strProp("远程文件路径"),
				"local_path":  strProp("本地保存路径"),
			}, []string{"remote_path", "local_path"}),
			Handler: toolSFTPDownload,
		},
		// ---- 检索 ----
		{
			Name: "grep", Description: "在远程主机递归搜索文件内容（grep -rn），支持 include 过滤与结果条数限制。",
			Parameters: schemaObj(map[string]interface{}{
				"pattern":   strProp("搜索模式（正则）"),
				"path":      strProp("搜索路径，默认当前目录"),
				"include":   strProp("文件名过滤，如 *.log（可选）"),
				"max_count": intProp("最大结果条数，默认 100，最大 500"),
			}, []string{"pattern"}),
			Handler: toolGrep,
		},
		{
			Name: "find", Description: "按名称/类型/修改时间查找远程文件（find），最多返回 200 条。",
			Parameters: schemaObj(map[string]interface{}{
				"path":  strProp("查找起始路径，默认当前目录"),
				"name":  strProp("文件名通配符，如 *.conf（可选）"),
				"type":  strProp("类型：f 文件 / d 目录（可选）"),
				"mtime": intProp("修改时间在最近 N 天内（可选）"),
			}, []string{}),
			Handler: toolFind,
		},
		{
			Name: "which_command", Description: "查询远程主机上某命令是否存在及其路径。",
			Parameters: schemaObj(map[string]interface{}{
				"name": strProp("命令名，如 nginx"),
			}, []string{"name"}),
			Handler: toolWhich,
		},
		{
			Name: "file_tail", Description: "查看远程文件末尾 N 行（tail -n），默认 50 行。",
			Parameters: schemaObj(map[string]interface{}{
				"path":  strProp("远程文件路径"),
				"lines": intProp("行数，默认 50，最大 2000"),
			}, []string{"path"}),
			Handler: toolFileTail,
		},
		{
			Name: "file_head", Description: "查看远程文件开头 N 行（head -n），默认 50 行。",
			Parameters: schemaObj(map[string]interface{}{
				"path":  strProp("远程文件路径"),
				"lines": intProp("行数，默认 50，最大 2000"),
			}, []string{"path"}),
			Handler: toolFileHead,
		},
		// ---- 系统运维 ----
		{
			Name:        "sys_info",
			Description: "获取系统基本信息（uname、发行版、运行时长）。",
			Parameters:  schemaObj(map[string]interface{}{}, []string{}),
			Handler:     toolSysInfo,
		},
		{
			Name:        "cpu_usage",
			Description: "查看 CPU 负载与占用前 10 的进程。",
			Parameters:  schemaObj(map[string]interface{}{}, []string{}),
			Handler:     toolCPUUsage,
		},
		{
			Name:        "mem_usage",
			Description: "查看内存使用（free -h）与内存占用前 10 的进程。",
			Parameters:  schemaObj(map[string]interface{}{}, []string{}),
			Handler:     toolMemUsage,
		},
		{
			Name: "disk_usage", Description: "查看磁盘使用（df -h）与 inode 使用情况。",
			Parameters: schemaObj(map[string]interface{}{
				"path": strProp("指定挂载点路径（可选）"),
			}, []string{}),
			Handler: toolDiskUsage,
		},
		{
			Name: "list_processes", Description: "列出进程（ps aux），可按 cpu 或 mem 排序。",
			Parameters: schemaObj(map[string]interface{}{
				"sort": strProp("排序字段：cpu（默认）或 mem"),
				"top":  intProp("显示前 N 个，默认 20，最大 100"),
			}, []string{}),
			Handler: toolListProcesses,
		},
		{
			Name: "kill_process", Description: "向指定进程发送信号（默认 SIGTERM）。高危操作，执行前需用户确认。",
			Parameters: schemaObj(map[string]interface{}{
				"pid":    intProp("进程 PID"),
				"signal": intProp("信号编号：15 默认 / 9 强杀 / 2 / 1"),
			}, []string{"pid"}),
			Handler: toolKillProcess,
		},
		{
			Name:        "list_services",
			Description: "列出系统服务（优先 systemctl，无 systemd 时退化 service --status-all）。",
			Parameters:  schemaObj(map[string]interface{}{}, []string{}),
			Handler:     toolListServices,
		},
		{
			Name: "service_action", Description: "对服务执行 start/stop/restart/status/reload/enable/disable。高危操作，执行前需用户确认。",
			Parameters: schemaObj(map[string]interface{}{
				"name":   strProp("服务名，如 nginx"),
				"action": strProp("start / stop / restart / status / reload / enable / disable"),
			}, []string{"name", "action"}),
			Handler: toolServiceAction,
		},
		{
			Name:        "list_ports",
			Description: "列出监听中的 TCP 端口（优先 ss，退化 netstat）。",
			Parameters:  schemaObj(map[string]interface{}{}, []string{}),
			Handler:     toolListPorts,
		},
		{
			Name: "http_check", Description: "从远程主机发起 HTTP 请求，返回状态码与耗时（curl）。",
			Parameters: schemaObj(map[string]interface{}{
				"url":     strProp("完整 URL，如 http://127.0.0.1:8080/health"),
				"timeout": intProp("超时秒数，默认 10，最大 60"),
			}, []string{"url"}),
			Handler: toolHTTPCheck,
		},
		{
			Name: "ping_host", Description: "从远程主机 ping 指定目标，检测网络连通性。",
			Parameters: schemaObj(map[string]interface{}{
				"host":  strProp("目标主机或 IP"),
				"count": intProp("次数，默认 4，最大 10"),
			}, []string{"host"}),
			Handler: toolPingHost,
		},
		{
			Name: "dns_lookup", Description: "在远程主机解析域名（getent / nslookup / dig 退化）。",
			Parameters: schemaObj(map[string]interface{}{
				"domain": strProp("要解析的域名"),
			}, []string{"domain"}),
			Handler: toolDNSLookup,
		},
		{
			Name: "tail_syslog", Description: "查看系统日志末尾 N 行（journalctl 或 /var/log 下常见日志），也可指定日志文件。",
			Parameters: schemaObj(map[string]interface{}{
				"lines": intProp("行数，默认 50，最大 500"),
				"file":  strProp("指定日志文件路径（可选），为空则读系统日志"),
			}, []string{}),
			Handler: toolTailSyslog,
		},
		{
			Name: "validate_config", Description: "校验服务配置文件语法，支持 nginx / apache / sshd / haproxy。",
			Parameters: schemaObj(map[string]interface{}{
				"service": strProp("服务名：nginx / apache / sshd / haproxy"),
				"config":  strProp("配置文件路径（haproxy 可选，默认 /etc/haproxy/haproxy.cfg）"),
			}, []string{"service"}),
			Handler: toolValidateConfig,
		},
		{
			Name: "list_cron", Description: "查看定时任务（当前用户 crontab、/etc/crontab、cron.d 等），可指定用户。",
			Parameters: schemaObj(map[string]interface{}{
				"user": strProp("指定用户名（可选），为空则查看当前用户及系统级任务"),
			}, []string{}),
			Handler: toolListCron,
		},
		// ---- 协作 ----
		{
			Name: "ask_user", Description: "向用户提问或请求确认（阻塞等待答复，120 秒超时视为拒绝）。执行危险或不确定的操作前必须调用。",
			Parameters: schemaObj(map[string]interface{}{
				"question": strProp("要向用户说明的问题或操作内容"),
				"risk":     strProp("风险说明，如 高/中/低 及原因"),
			}, []string{"question"}),
			Handler: toolAskUser,
		},
		{
			Name: "delegate_subtask", Description: "将独立子任务委派给子代理执行（共享当前主机连接），返回子任务结果文本。嵌套深度最多 2 层。",
			Parameters: schemaObj(map[string]interface{}{
				"task": strProp("子任务的完整描述，需包含足够上下文"),
			}, []string{"task"}),
			Handler: toolDelegateSubtask,
		},
	}
}
