//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// runningExePath 返回需要被替换的运行中程序路径
func runningExePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// launchUpdater 以独立进程启动 PowerShell 更新脚本（不随主程序退出）
func launchUpdater(scriptPath string) error {
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass",
		"-WindowStyle", "Hidden", "-File", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // DETACHED_PROCESS
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动更新程序失败: %v", err)
	}
	return nil
}

// writeUpdateScript 生成 PowerShell 更新脚本：等待本进程退出 -> 替换 exe -> 重启
// 替换遇文件锁（杀毒软件扫描等）自动重试 40 次；始终写日志；最终失败则回退启动旧版，避免程序消失
// 以 UTF-8 BOM 保存，确保中文路径被正确解析
func writeUpdateScript(exePath, newPath string, pid int) (string, error) {
	content := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$old = '%s'
$new = '%s'
$targetPid = %d
$log = Join-Path $env:TEMP 'lindong-ssh-update.log'
function Log($m) { Add-Content -LiteralPath $log -Encoding UTF8 -Value ((Get-Date -Format 'yyyy-MM-dd HH:mm:ss') + ' ' + $m) }
try {
  Log ('update start pid=' + $targetPid)
  $deadline = (Get-Date).AddSeconds(60)
  while ((Get-Process -Id $targetPid -ErrorAction SilentlyContinue) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
  Start-Sleep -Milliseconds 800
  $ok = $false
  for ($i = 1; $i -le 40; $i++) {
    try {
      Move-Item -Force -LiteralPath $new -Destination $old -ErrorAction Stop
      $ok = $true; break
    } catch {
      Log ('replace try ' + $i + ' failed: ' + $_.Exception.Message)
      Start-Sleep -Milliseconds 1000
    }
  }
  if ($ok) {
    Log 'replace ok, starting new version'
    Start-Process -FilePath $old
  } else {
    Log 'replace FAILED, fallback start old version'
    Start-Process -FilePath $old
  }
} catch { Log ('fatal: ' + $_.Exception.Message) }
Remove-Item -LiteralPath $PSCommandPath -Force -ErrorAction SilentlyContinue
`, strings.ReplaceAll(exePath, "'", "''"), strings.ReplaceAll(newPath, "'", "''"), pid)

	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("lindong-ssh-update-%d.ps1", pid))
	bom := []byte{0xEF, 0xBB, 0xBF}
	if err := os.WriteFile(scriptPath, append(bom, []byte(content)...), 0644); err != nil {
		return "", err
	}
	return scriptPath, nil
}
