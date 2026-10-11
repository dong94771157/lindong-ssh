//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// runningExePath 返回需要被替换的运行中程序路径。
// Linux 下 AppImage 运行时 os.Executable() 指向 /tmp 的 FUSE 挂载点，
// 真正需要替换的是 APPIMAGE 环境变量给出的镜像文件本身。
func runningExePath() (string, error) {
	if runtime.GOOS == "linux" {
		if a := strings.TrimSpace(os.Getenv("APPIMAGE")); a != "" {
			if _, err := os.Stat(a); err == nil {
				return a, nil
			}
		}
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// launchUpdater 以新会话启动 bash 更新脚本（不随主程序退出）
func launchUpdater(scriptPath string) error {
	cmd := exec.Command("/bin/bash", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动更新程序失败: %v", err)
	}
	return nil
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// writeUpdateScript 生成 bash 更新脚本：等待本进程退出 -> 替换可执行文件 -> 重启。
// 适用于 Linux（含 AppImage）与 macOS（.app 内的可执行文件）。
// 替换重试 40 次；始终写日志；最终失败也会重启旧版本，避免程序消失。
func writeUpdateScript(exePath, newPath string, pid int) (string, error) {
	content := fmt.Sprintf(`#!/bin/bash
old=%s
new=%s
target_pid=%d
log=%s
logf(){ echo "$(date '+%%Y-%%m-%%d %%H:%%M:%%S') $1" >> "$log"; }
for _ in $(seq 1 120); do
  kill -0 "$target_pid" 2>/dev/null || break
  sleep 0.5
done
sleep 0.8
ok=0
for i in $(seq 1 40); do
  if mv -f "$new" "$old" 2>>"$log"; then
    chmod 0755 "$old"
    ok=1
    break
  fi
  logf "replace try $i failed"
  sleep 1
done
if [ "$ok" = "1" ]; then
  logf "replace ok, starting new version"
else
  logf "replace FAILED, fallback start old version"
fi
nohup "$old" >/dev/null 2>&1 &
rm -f -- "$0"
`, shQuote(exePath), shQuote(newPath), pid, shQuote(updateLogPath()))

	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("lindong-ssh-update-%d.sh", pid))
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		return "", err
	}
	return scriptPath, nil
}
