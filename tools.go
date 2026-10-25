package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/sftp"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/crypto/ssh"
)

// ---------------- 工具执行上下文 ----------------

// agentCtx 一次 Agent 任务中工具执行所需的上下文
type agentCtx struct {
	app       *App
	agentID   string
	sessionID string
	depth     int // 子代理嵌套深度，0 为顶层
}

// ---------------- 参数解析辅助 ----------------

func argStr(args map[string]interface{}, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// argStrRaw 不去除首尾空白（用于文件内容、脚本等）
func argStrRaw(args map[string]interface{}, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argInt(args map[string]interface{}, key string, def int) int {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return def
}

// truncateStr 截断过长输出，保留首尾
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max < 100 {
		return s[:max]
	}
	half := max / 2
	return s[:half] + fmt.Sprintf("\n...（中间省略 %d 字节）...\n", len(s)-max) + s[len(s)-half:]
}

// shellQuote 单引号包裹防注入
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------- 会话访问 ----------------

// sshClient 从交互会话获取底层 SSH 连接
func (c *agentCtx) sshClient() (*ssh.Client, error) {
	s := c.app.getSession(c.sessionID)
	if s == nil {
		return nil, errors.New("终端会话不存在或已断开，请重新连接主机")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, errors.New("终端会话已关闭，请重新连接主机")
	}
	return s.client, nil
}

// sftpClient 基于当前会话新建 SFTP 客户端（调用方负责 Close）
func (c *agentCtx) sftpClient() (*sftp.Client, error) {
	client, err := c.sshClient()
	if err != nil {
		return nil, err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("初始化 SFTP 失败: %v", err)
	}
	return sc, nil
}

// interactiveSession 获取交互式终端会话
func (c *agentCtx) interactiveSession() (*SSHSession, error) {
	s := c.app.getSession(c.sessionID)
	if s == nil {
		return nil, errors.New("终端会话不存在或已断开，请重新连接主机")
	}
	return s, nil
}

// execCommand 新建独立 exec 会话执行命令，带超时与输出截断（stdin 可为 nil）
func (c *agentCtx) execCommand(cmd string, stdin io.Reader, timeout time.Duration) (string, error) {
	client, err := c.sshClient()
	if err != nil {
		return "", err
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("创建执行会话失败: %v", err)
	}
	defer sess.Close()
	if stdin != nil {
		sess.Stdin = stdin
	}

	type execResult struct {
		out []byte
		err error
	}
	ch := make(chan execResult, 1)
	go func() {
		out, err := sess.CombinedOutput(cmd)
		ch <- execResult{out, err}
	}()
	select {
	case r := <-ch:
		out := truncateStr(string(r.out), 32*1024)
		if r.err != nil {
			if out == "" {
				return "", fmt.Errorf("命令执行失败: %v", r.err)
			}
			return out + fmt.Sprintf("\n（退出状态异常: %v）", r.err), nil
		}
		if strings.TrimSpace(out) == "" {
			return "（命令执行成功，无输出）", nil
		}
		return out, nil
	case <-time.After(timeout):
		_ = sess.Close()
		return "", fmt.Errorf("命令执行超时（%v），已强制终止", timeout)
	}
}

// ---------------- 高危命令拦截 ----------------

var highRiskPatterns = []*regexp.Regexp{
	regexp.MustCompile(`rm\s+-[a-zA-Z]*[rf]{2,}[a-zA-Z]*\s+/`),
	regexp.MustCompile(`rm\s+-[a-zA-Z]*r[a-zA-Z]*\s+/\s*$`),
	regexp.MustCompile(`\bmkfs(\.[a-z0-9]+)?\b`),
	regexp.MustCompile(`\bdd\s+.*\bif=`),
	regexp.MustCompile(`:\(\)\s*\{\s*:\|:&\s*\}\s*;:`),
	regexp.MustCompile(`\b(shutdown|poweroff|reboot|halt|init\s+0|init\s+6)\b`),
	regexp.MustCompile(`iptables\s+-F`),
	regexp.MustCompile(`chmod\s+-R\s+777\s+/`),
	regexp.MustCompile(`>\s*/dev/sd[a-z]`),
	regexp.MustCompile(`\bkill\s+-9\s+-1\b`),
	regexp.MustCompile(`\b(mkfs|mkswap)\s+/dev/`),
	regexp.MustCompile(`\buserdel\s+-r\s+root\b`),
}

// assessRisk 评估命令风险等级，返回是否高危及命中的规则
func assessRisk(cmd string) (bool, string) {
	for _, re := range highRiskPatterns {
		if re.MatchString(cmd) {
			return true, re.String()
		}
	}
	return false, ""
}

// alwaysConfirmTools 无论参数如何都必须用户确认的工具
var alwaysConfirmTools = map[string]bool{
	"kill_process":   true,
	"service_action": true,
	"sftp_delete":    true,
}

// toolRiskTarget 提取需要风险评估的操作描述；第二个返回值表示该工具需要过风险评估
func toolRiskTarget(name string, args map[string]interface{}) (string, bool) {
	switch name {
	case "run_command":
		return argStr(args, "command"), true
	case "run_script":
		return argStrRaw(args, "script"), true
	case "write_terminal":
		return argStrRaw(args, "data"), true
	case "kill_process":
		return fmt.Sprintf("结束进程 pid=%d signal=%d", argInt(args, "pid", 0), argInt(args, "signal", 15)), true
	case "service_action":
		return "服务操作 " + argStr(args, "action") + " " + argStr(args, "name"), true
	case "sftp_delete":
		return "删除远程路径 " + argStr(args, "path"), true
	}
	return "", false
}

// ---------------- 命令执行类工具 ----------------

func toolRunCommand(c *agentCtx, args map[string]interface{}) (string, error) {
	cmd := argStr(args, "command")
	if cmd == "" {
		return "", errors.New("缺少 command 参数")
	}
	timeout := time.Duration(argInt(args, "timeout", 60)) * time.Second
	if timeout <= 0 || timeout > 300*time.Second {
		timeout = 60 * time.Second
	}
	return c.execCommand(cmd, nil, timeout)
}

func toolRunScript(c *agentCtx, args map[string]interface{}) (string, error) {
	script := argStrRaw(args, "script")
	if strings.TrimSpace(script) == "" {
		return "", errors.New("缺少 script 参数")
	}
	shell := argStr(args, "shell")
	if shell != "sh" {
		shell = "bash"
	}
	timeout := time.Duration(argInt(args, "timeout", 60)) * time.Second
	if timeout <= 0 || timeout > 300*time.Second {
		timeout = 60 * time.Second
	}
	// 通过 stdin 传入脚本，避免多行转义问题
	return c.execCommand(shell, strings.NewReader(script), timeout)
}

// pendingInteractiveRe 匹配终端正在等待人工敏感输入的提示（密码/sudo/passphrase/MOK 等）
var pendingInteractiveRe = regexp.MustCompile(`(?im)(\[sudo\] password|password|密码|passphrase|mokey|MOK password)\s*(for[^\n:]*)?[:：]\s*$`)

func toolWriteTerminal(c *agentCtx, args map[string]interface{}) (string, error) {
	data := argStrRaw(args, "data")
	if data == "" {
		return "", errors.New("缺少 data 参数")
	}
	s, err := c.interactiveSession()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("终端会话已关闭")
	}
	// 交互转交：终端正在等待密码类输入时，不代为写入，转为通知用户人工完成
	tail := s.recentOut
	if len(tail) > 2048 {
		tail = tail[len(tail)-2048:]
	}
	if loc := pendingInteractiveRe.FindStringIndex(string(tail)); loc != nil {
		promptTail := strings.TrimSpace(string(tail[loc[0]:]))
		wailsRuntime.EventsEmit(c.app.ctx, "agent:interactive:"+c.agentID, map[string]string{
			"sessionId": c.sessionID,
			"prompt":    truncateStr(promptTail, 200),
		})
		return "检测到终端正在等待敏感交互输入（" + truncateStr(promptTail, 80) + "）。出于安全考虑 Agent 不代为输入密码，已通过界面请求用户在交互终端中人工完成。请调用 ask_user 引导用户输入后继续。", nil
	}
	if _, err := s.stdin.Write([]byte(data)); err != nil {
		return "", fmt.Errorf("写入终端失败: %v", err)
	}
	return fmt.Sprintf("已写入终端 stdin（%d 字节），如需观察结果请稍后调用 read_terminal", len(data)), nil
}

func toolReadTerminal(c *agentCtx, args map[string]interface{}) (string, error) {
	s, err := c.interactiveSession()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	out := string(s.recentOut)
	s.mu.Unlock()
	out = strings.TrimRight(out, "\x00")
	if strings.TrimSpace(out) == "" {
		return "（终端暂无输出缓冲）", nil
	}
	return truncateStr(out, 16*1024), nil
}

// ---------------- 文件 / SFTP 类工具 ----------------

func toolSFTPRead(c *agentCtx, args map[string]interface{}) (string, error) {
	p := argStr(args, "path")
	if p == "" {
		return "", errors.New("缺少 path 参数")
	}
	sc, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	defer sc.Close()
	f, err := sc.Open(p)
	if err != nil {
		return "", fmt.Errorf("打开远程文件失败: %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil {
		return "", fmt.Errorf("读取远程文件失败: %v", err)
	}
	out := string(data)
	if len(out) > 256*1024 {
		out = out[:256*1024] + "\n...（文件过大，已截断至 256KB）"
	}
	return out, nil
}

func toolSFTPWrite(c *agentCtx, args map[string]interface{}) (string, error) {
	p := argStr(args, "path")
	content := argStrRaw(args, "content")
	if p == "" {
		return "", errors.New("缺少 path 参数")
	}
	sc, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	defer sc.Close()
	// 写前自动备份原文件
	backup := ""
	if _, err := sc.Stat(p); err == nil {
		backup = fmt.Sprintf("%s.bak-%s", p, time.Now().Format("20060102-150405"))
		if err := sc.Rename(p, backup); err != nil {
			return "", fmt.Errorf("备份原文件失败: %v", err)
		}
	}
	dir := path.Dir(p)
	if dir != "." && dir != "/" {
		_ = sc.MkdirAll(dir)
	}
	f, err := sc.Create(p)
	if err != nil {
		return "", fmt.Errorf("创建远程文件失败: %v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte(content)); err != nil {
		return "", fmt.Errorf("写入远程文件失败: %v", err)
	}
	if backup != "" {
		return fmt.Sprintf("写入成功（%d 字节），原文件已备份为 %s", len(content), backup), nil
	}
	return fmt.Sprintf("写入成功（%d 字节）", len(content)), nil
}

func toolSFTPList(c *agentCtx, args map[string]interface{}) (string, error) {
	p := argStr(args, "path")
	if p == "" {
		p = "."
	}
	sc, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	defer sc.Close()
	entries, err := sc.ReadDir(p)
	if err != nil {
		return "", fmt.Errorf("读取目录失败: %v", err)
	}
	if len(entries) == 0 {
		return "（空目录）", nil
	}
	var sb strings.Builder
	for i, e := range entries {
		if i >= 200 {
			sb.WriteString(fmt.Sprintf("...（共 %d 项，仅显示前 200 项）\n", len(entries)))
			break
		}
		sb.WriteString(fmt.Sprintf("%s %12d %s %s\n",
			e.Mode().String(), e.Size(), e.ModTime().Format("2006-01-02 15:04"), e.Name()))
	}
	return sb.String(), nil
}

func toolSFTPStat(c *agentCtx, args map[string]interface{}) (string, error) {
	p := argStr(args, "path")
	if p == "" {
		return "", errors.New("缺少 path 参数")
	}
	sc, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	defer sc.Close()
	fi, err := sc.Stat(p)
	if err != nil {
		return "", fmt.Errorf("获取文件信息失败: %v", err)
	}
	kind := "文件"
	if fi.IsDir() {
		kind = "目录"
	}
	return fmt.Sprintf("路径: %s\n类型: %s\n大小: %d 字节\n权限: %s\n修改时间: %s",
		p, kind, fi.Size(), fi.Mode().String(), fi.ModTime().Format("2006-01-02 15:04:05")), nil
}

func toolSFTPMkdir(c *agentCtx, args map[string]interface{}) (string, error) {
	p := argStr(args, "path")
	if p == "" {
		return "", errors.New("缺少 path 参数")
	}
	sc, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	defer sc.Close()
	if err := sc.MkdirAll(p); err != nil {
		return "", fmt.Errorf("创建目录失败: %v", err)
	}
	return "目录创建成功: " + p, nil
}

func toolSFTPDelete(c *agentCtx, args map[string]interface{}) (string, error) {
	p := argStr(args, "path")
	if p == "" {
		return "", errors.New("缺少 path 参数")
	}
	if p == "/" || p == "/*" {
		return "", errors.New("禁止删除根目录")
	}
	sc, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	defer sc.Close()
	fi, err := sc.Stat(p)
	if err != nil {
		return "", fmt.Errorf("目标不存在: %v", err)
	}
	if fi.IsDir() {
		if err := removeAllSFTP(sc, p); err != nil {
			return "", fmt.Errorf("删除目录失败: %v", err)
		}
		return "目录已递归删除: " + p, nil
	}
	if err := sc.Remove(p); err != nil {
		return "", fmt.Errorf("删除文件失败: %v", err)
	}
	return "文件已删除: " + p, nil
}

// removeAllSFTP 递归删除 SFTP 目录
func removeAllSFTP(sc *sftp.Client, p string) error {
	entries, err := sc.ReadDir(p)
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := path.Join(p, e.Name())
		if e.IsDir() {
			if err := removeAllSFTP(sc, child); err != nil {
				return err
			}
		} else {
			if err := sc.Remove(child); err != nil {
				return err
			}
		}
	}
	return sc.RemoveDirectory(p)
}

func toolSFTPRename(c *agentCtx, args map[string]interface{}) (string, error) {
	oldPath := argStr(args, "old_path")
	newPath := argStr(args, "new_path")
	if oldPath == "" || newPath == "" {
		return "", errors.New("缺少 old_path 或 new_path 参数")
	}
	sc, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	defer sc.Close()
	if err := sc.Rename(oldPath, newPath); err != nil {
		return "", fmt.Errorf("重命名失败: %v", err)
	}
	return fmt.Sprintf("已重命名: %s → %s", oldPath, newPath), nil
}

func toolSFTPUpload(c *agentCtx, args map[string]interface{}) (string, error) {
	local := argStr(args, "local_path")
	remote := argStr(args, "remote_path")
	if local == "" || remote == "" {
		return "", errors.New("缺少 local_path 或 remote_path 参数")
	}
	f, err := os.Open(local)
	if err != nil {
		return "", fmt.Errorf("打开本地文件失败: %v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > 100*1024*1024 {
		return "", errors.New("文件超过 100MB，请改用界面的文件上传功能")
	}
	sc, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	defer sc.Close()
	remote = filepathSlash(remote)
	dir := path.Dir(remote)
	if dir != "." && dir != "/" {
		_ = sc.MkdirAll(dir)
	}
	out, err := sc.Create(remote)
	if err != nil {
		return "", fmt.Errorf("创建远程文件失败: %v", err)
	}
	defer out.Close()
	n, err := io.Copy(out, f)
	if err != nil {
		return "", fmt.Errorf("写入远程文件失败: %v", err)
	}
	return fmt.Sprintf("上传成功: %s → %s（%d 字节）", local, remote, n), nil
}

func toolSFTPDownload(c *agentCtx, args map[string]interface{}) (string, error) {
	remote := argStr(args, "remote_path")
	local := argStr(args, "local_path")
	if remote == "" || local == "" {
		return "", errors.New("缺少 remote_path 或 local_path 参数")
	}
	sc, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	defer sc.Close()
	src, err := sc.Open(remote)
	if err != nil {
		return "", fmt.Errorf("打开远程文件失败: %v", err)
	}
	defer src.Close()
	dst, err := os.Create(local)
	if err != nil {
		return "", fmt.Errorf("创建本地文件失败: %v", err)
	}
	defer dst.Close()
	n, err := io.Copy(dst, src)
	if err != nil {
		return "", fmt.Errorf("下载失败: %v", err)
	}
	return fmt.Sprintf("下载成功: %s → %s（%d 字节）", remote, local, n), nil
}

// ---------------- 检索类工具 ----------------

func toolGrep(c *agentCtx, args map[string]interface{}) (string, error) {
	pattern := argStr(args, "pattern")
	if pattern == "" {
		return "", errors.New("缺少 pattern 参数")
	}
	searchPath := argStr(args, "path")
	if searchPath == "" {
		searchPath = "."
	}
	maxCount := argInt(args, "max_count", 100)
	if maxCount <= 0 || maxCount > 500 {
		maxCount = 100
	}
	cmd := "grep -rn --color=never"
	if include := argStr(args, "include"); include != "" {
		cmd += " --include=" + shellQuote(include)
	}
	cmd += fmt.Sprintf(" -m %d %s %s 2>/dev/null | head -n %d", maxCount, shellQuote(pattern), shellQuote(searchPath), maxCount)
	return c.execCommand(cmd, nil, 30*time.Second)
}

func toolFind(c *agentCtx, args map[string]interface{}) (string, error) {
	searchPath := argStr(args, "path")
	if searchPath == "" {
		searchPath = "."
	}
	cmd := "find " + shellQuote(searchPath)
	if name := argStr(args, "name"); name != "" {
		cmd += " -name " + shellQuote(name)
	}
	if ftype := argStr(args, "type"); ftype == "f" || ftype == "d" {
		cmd += " -type " + ftype
	}
	if mtime := argInt(args, "mtime", 0); mtime > 0 {
		cmd += fmt.Sprintf(" -mtime -%d", mtime)
	}
	cmd += " 2>/dev/null | head -n 200"
	return c.execCommand(cmd, nil, 30*time.Second)
}

func toolWhich(c *agentCtx, args map[string]interface{}) (string, error) {
	name := argStr(args, "name")
	if name == "" {
		return "", errors.New("缺少 name 参数")
	}
	q := shellQuote(name)
	cmd := "command -v " + q + " 2>/dev/null || which " + q + " 2>/dev/null || echo 'NOT_FOUND'"
	return c.execCommand(cmd, nil, 15*time.Second)
}

func toolFileTail(c *agentCtx, args map[string]interface{}) (string, error) {
	p := argStr(args, "path")
	if p == "" {
		return "", errors.New("缺少 path 参数")
	}
	n := argInt(args, "lines", 50)
	if n <= 0 || n > 2000 {
		n = 50
	}
	cmd := fmt.Sprintf("tail -n %d %s", n, shellQuote(p))
	return c.execCommand(cmd, nil, 15*time.Second)
}

func toolFileHead(c *agentCtx, args map[string]interface{}) (string, error) {
	p := argStr(args, "path")
	if p == "" {
		return "", errors.New("缺少 path 参数")
	}
	n := argInt(args, "lines", 50)
	if n <= 0 || n > 2000 {
		n = 50
	}
	cmd := fmt.Sprintf("head -n %d %s", n, shellQuote(p))
	return c.execCommand(cmd, nil, 15*time.Second)
}

// ---------------- 系统运维类工具 ----------------

func toolSysInfo(c *agentCtx, args map[string]interface{}) (string, error) {
	cmd := "echo '=== uname ==='; uname -a; echo; echo '=== os-release ==='; head -n 6 /etc/os-release 2>/dev/null; echo; echo '=== uptime ==='; uptime"
	return c.execCommand(cmd, nil, 15*time.Second)
}

func toolCPUUsage(c *agentCtx, args map[string]interface{}) (string, error) {
	cmd := "top -bn1 2>/dev/null | head -n 5; echo; echo '=== CPU 占用前 10 进程 ==='; ps aux --sort=-%cpu 2>/dev/null | head -n 11 || ps aux | head -n 11"
	return c.execCommand(cmd, nil, 20*time.Second)
}

func toolMemUsage(c *agentCtx, args map[string]interface{}) (string, error) {
	cmd := "free -h; echo; echo '=== 内存占用前 10 进程 ==='; ps aux --sort=-%mem 2>/dev/null | head -n 11 || ps aux | head -n 11"
	return c.execCommand(cmd, nil, 15*time.Second)
}

func toolDiskUsage(c *agentCtx, args map[string]interface{}) (string, error) {
	cmd := "df -h"
	if p := argStr(args, "path"); p != "" {
		cmd += " " + shellQuote(p)
	}
	cmd += "; echo; echo '=== inode 使用 ==='; df -i 2>/dev/null | head -n 20"
	return c.execCommand(cmd, nil, 15*time.Second)
}

func toolListProcesses(c *agentCtx, args map[string]interface{}) (string, error) {
	top := argInt(args, "top", 20)
	if top <= 0 || top > 100 {
		top = 20
	}
	key := "-%cpu"
	if argStr(args, "sort") == "mem" {
		key = "-%mem"
	}
	cmd := fmt.Sprintf("ps aux --sort=%s 2>/dev/null | head -n %d || ps aux | head -n %d", key, top+1, top+1)
	return c.execCommand(cmd, nil, 15*time.Second)
}

func toolKillProcess(c *agentCtx, args map[string]interface{}) (string, error) {
	pid := argInt(args, "pid", 0)
	if pid <= 0 {
		return "", errors.New("缺少有效的 pid 参数")
	}
	sig := argInt(args, "signal", 15)
	switch sig {
	case 1, 2, 9, 15:
	default:
		sig = 15
	}
	cmd := fmt.Sprintf("kill -%d %d && echo '已向进程 %d 发送信号 %d' || echo '发送失败，进程可能不存在或无权限'", sig, pid, pid, sig)
	return c.execCommand(cmd, nil, 15*time.Second)
}

func toolListServices(c *agentCtx, args map[string]interface{}) (string, error) {
	cmd := "if command -v systemctl >/dev/null 2>&1; then systemctl list-units --type=service --no-pager --no-legend 2>/dev/null | head -n 60; else service --status-all 2>&1 | head -n 60; fi"
	return c.execCommand(cmd, nil, 20*time.Second)
}

func toolServiceAction(c *agentCtx, args map[string]interface{}) (string, error) {
	name := argStr(args, "name")
	action := argStr(args, "action")
	if name == "" {
		return "", errors.New("缺少 name 参数")
	}
	switch action {
	case "start", "stop", "restart", "status", "reload", "enable", "disable":
	default:
		return "", errors.New("action 仅支持 start/stop/restart/status/reload/enable/disable")
	}
	q := shellQuote(name)
	var cmd string
	if action == "enable" || action == "disable" {
		cmd = "systemctl " + action + " " + q + " 2>&1"
	} else {
		cmd = "if command -v systemctl >/dev/null 2>&1; then systemctl " + action + " " + q + " 2>&1; "
		if action == "status" {
			cmd += "else service " + q + " status 2>&1; fi"
		} else {
			cmd += "else service " + q + " " + action + " 2>&1; fi"
		}
		if action == "status" || action == "restart" {
			cmd += "; sleep 1; (systemctl is-active " + q + " 2>/dev/null || true)"
		}
	}
	return c.execCommand(cmd, nil, 60*time.Second)
}

func toolListPorts(c *agentCtx, args map[string]interface{}) (string, error) {
	cmd := "if command -v ss >/dev/null 2>&1; then ss -tlnp 2>/dev/null || ss -tln; else netstat -tlnp 2>/dev/null || netstat -tln; fi | head -n 80"
	return c.execCommand(cmd, nil, 15*time.Second)
}

func toolHTTPCheck(c *agentCtx, args map[string]interface{}) (string, error) {
	url := argStr(args, "url")
	if url == "" {
		return "", errors.New("缺少 url 参数")
	}
	timeoutSec := argInt(args, "timeout", 10)
	if timeoutSec <= 0 || timeoutSec > 60 {
		timeoutSec = 10
	}
	cmd := fmt.Sprintf("curl -o /dev/null -s -m %d -w 'HTTP状态码: %%{http_code}\\n总耗时: %%{time_total}s\\n连接耗时: %%{time_connect}s\\n' %s || echo '请求失败'", timeoutSec, shellQuote(url))
	return c.execCommand(cmd, nil, time.Duration(timeoutSec+15)*time.Second)
}

func toolPingHost(c *agentCtx, args map[string]interface{}) (string, error) {
	host := argStr(args, "host")
	if host == "" {
		return "", errors.New("缺少 host 参数")
	}
	count := argInt(args, "count", 4)
	if count <= 0 || count > 10 {
		count = 4
	}
	cmd := fmt.Sprintf("ping -c %d -W 3 %s", count, shellQuote(host))
	return c.execCommand(cmd, nil, time.Duration(count*3+15)*time.Second)
}

func toolDNSLookup(c *agentCtx, args map[string]interface{}) (string, error) {
	domain := argStr(args, "domain")
	if domain == "" {
		return "", errors.New("缺少 domain 参数")
	}
	q := shellQuote(domain)
	cmd := "if command -v getent >/dev/null 2>&1; then getent hosts " + q + "; fi; "
	cmd += "if command -v nslookup >/dev/null 2>&1; then nslookup " + q + " 2>&1; "
	cmd += "elif command -v dig >/dev/null 2>&1; then dig +short " + q + " 2>&1; fi"
	return c.execCommand(cmd, nil, 20*time.Second)
}

func toolTailSyslog(c *agentCtx, args map[string]interface{}) (string, error) {
	n := argInt(args, "lines", 50)
	if n <= 0 || n > 500 {
		n = 50
	}
	var cmd string
	if logFile := argStr(args, "file"); logFile != "" {
		cmd = fmt.Sprintf("tail -n %d %s", n, shellQuote(logFile))
	} else {
		cmd = fmt.Sprintf("if command -v journalctl >/dev/null 2>&1; then journalctl -n %d --no-pager 2>/dev/null; ", n)
		cmd += fmt.Sprintf("elif [ -f /var/log/syslog ]; then tail -n %d /var/log/syslog; ", n)
		cmd += fmt.Sprintf("elif [ -f /var/log/messages ]; then tail -n %d /var/log/messages; ", n)
		cmd += "else echo '未找到系统日志'; fi"
	}
	return c.execCommand(cmd, nil, 20*time.Second)
}

func toolValidateConfig(c *agentCtx, args map[string]interface{}) (string, error) {
	svc := strings.ToLower(argStr(args, "service"))
	var cmd string
	switch svc {
	case "nginx":
		cmd = "nginx -t 2>&1"
	case "apache", "httpd", "apache2":
		cmd = "(apachectl configtest || apache2ctl configtest || httpd -t) 2>&1"
	case "sshd", "ssh":
		cmd = "sshd -t 2>&1 && echo 'sshd 配置语法正确' || echo 'sshd 配置检查未通过'"
	case "haproxy":
		cfg := argStr(args, "config")
		if cfg == "" {
			cfg = "/etc/haproxy/haproxy.cfg"
		}
		cmd = "haproxy -c -f " + shellQuote(cfg) + " 2>&1"
	default:
		return "", errors.New("支持的服务: nginx / apache / sshd / haproxy")
	}
	return c.execCommand(cmd, nil, 30*time.Second)
}

func toolListCron(c *agentCtx, args map[string]interface{}) (string, error) {
	var cmd string
	if user := argStr(args, "user"); user != "" {
		cmd = "crontab -u " + shellQuote(user) + " -l 2>&1"
	} else {
		cmd = "echo '=== 当前用户 crontab ==='; crontab -l 2>&1; echo; echo '=== /etc/crontab ==='; cat /etc/crontab 2>/dev/null; echo; echo '=== /etc/cron.d ==='; ls /etc/cron.d 2>/dev/null; echo; echo '=== 周期目录 ==='; ls /etc/cron.daily /etc/cron.hourly /etc/cron.weekly /etc/cron.monthly 2>/dev/null"
	}
	return c.execCommand(cmd, nil, 15*time.Second)
}

// ---------------- 协作类工具 ----------------

// askResponse 用户对确认请求的答复
type askResponse struct {
	Approved bool   `json:"approved"`
	Note     string `json:"note"`
}

func toolAskUser(c *agentCtx, args map[string]interface{}) (string, error) {
	question := argStr(args, "question")
	if question == "" {
		return "", errors.New("缺少 question 参数")
	}
	risk := argStr(args, "risk")
	resp, err := c.app.askUserAndWait(c.agentID, question, risk, 120*time.Second)
	if err != nil {
		return "", err
	}
	if !resp.Approved {
		if resp.Note != "" {
			return "用户拒绝了该操作，补充说明：" + resp.Note, nil
		}
		return "用户拒绝了该操作", nil
	}
	if resp.Note != "" {
		return "用户已批准该操作，补充说明：" + resp.Note, nil
	}
	return "用户已批准该操作", nil
}

func toolDelegateSubtask(c *agentCtx, args map[string]interface{}) (string, error) {
	task := argStr(args, "task")
	if task == "" {
		return "", errors.New("缺少 task 参数")
	}
	if c.depth >= agentMaxDepth {
		return "", errors.New("子代理嵌套深度已达上限，请直接执行该子任务")
	}
	// 子代理共享同一终端会话与调用计数器，静默执行并直接返回文本结果
	result, err := c.app.runAgentLoop(c.agentID, c.sessionID, nil, task, c.depth+1, true)
	if err != nil {
		return "", fmt.Errorf("子代理执行失败: %v", err)
	}
	return "子代理任务完成，结果如下：\n" + truncateStr(result, 8*1024), nil
}
