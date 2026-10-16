//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ConnectRDP 生成 .rdp 文件并调起系统远程桌面（mstsc）连接 Windows 服务器
// 仅 Windows 平台可用
func (a *App) ConnectRDP(serverID string) error {
	sv, ok := a.store.get(serverID)
	if !ok {
		return fmt.Errorf("服务器不存在")
	}

	port := sv.Port
	if port <= 0 {
		port = 3389
	}
	host := strings.TrimSpace(sv.Host)
	if host == "" {
		a.addLog(rdpConnectLog(sv, port, "fail", "主机地址为空"))
		return fmt.Errorf("主机地址为空")
	}

	rdpPath := filepath.Join(os.TempDir(), "lindong-rdp-"+sv.ID+".rdp")
	if err := os.WriteFile(rdpPath, []byte(buildRDPFile(host, port, sv.Username)), 0644); err != nil {
		msg := "生成远程桌面配置失败: " + err.Error()
		a.addLog(rdpConnectLog(sv, port, "fail", msg))
		return fmt.Errorf("%s", msg)
	}

	// 预置凭据以便免密登录（失败不阻断，mstsc 会回落到手动输入）
	if sv.Password != "" {
		_ = exec.Command("cmdkey", "/generic:TERMSRV/"+host, "/user:"+sv.Username, "/pass:"+sv.Password).Run()
	}

	cmd := exec.Command("mstsc", rdpPath)
	// 独立进程，避免随本程序退出而结束
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008} // DETACHED_PROCESS
	if err := cmd.Start(); err != nil {
		os.Remove(rdpPath)
		msg := "启动远程桌面失败: " + err.Error()
		a.addLog(rdpConnectLog(sv, port, "fail", msg))
		return fmt.Errorf("%s", msg)
	}

	a.addLog(rdpConnectLog(sv, port, "success", "已打开远程桌面"))
	// mstsc 读取配置后即可清理临时文件
	go func() {
		time.Sleep(20 * time.Second)
		_ = os.Remove(rdpPath)
	}()
	return nil
}

// buildRDPFile 生成 .rdp 配置内容（Windows 换行）
func buildRDPFile(host string, port int, username string) string {
	var b strings.Builder
	b.WriteString("screen mode id:i:2\r\n")
	b.WriteString("use multimon:i:0\r\n")
	b.WriteString("full address:s:" + host + ":" + strconv.Itoa(port) + "\r\n")
	if username != "" {
		b.WriteString("username:s:" + username + "\r\n")
	}
	b.WriteString("prompt for credentials:i:1\r\n")
	b.WriteString("authentication level:i:2\r\n")
	b.WriteString("audiomode:i:0\r\n")
	b.WriteString("redirectclipboard:i:1\r\n")
	return b.String()
}

// rdpConnectLog 构造一条 RDP 连接日志
func rdpConnectLog(sv Server, port int, status, msg string) ConnLog {
	return ConnLog{
		ServerID: sv.ID, Name: sv.Name, Host: sv.Host, Port: port,
		Username: sv.Username, Protocol: "rdp", Event: "connect",
		Status: status, Message: msg,
	}
}
