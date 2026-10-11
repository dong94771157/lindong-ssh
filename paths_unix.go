//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// dataDir Linux 遵循 XDG_CONFIG_HOME（默认 ~/.config/lindong-ssh），
// macOS 使用 ~/Library/Application Support/lindong-ssh。
// 程序位于 .app 包或 /usr 内时这些位置同样可写。
func dataDir() string {
	var dir string
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, "Library", "Application Support", "lindong-ssh")
		}
	default:
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			dir = filepath.Join(x, "lindong-ssh")
		} else if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".config", "lindong-ssh")
		}
	}
	if dir == "" {
		if exePath, err := os.Executable(); err == nil {
			dir = filepath.Dir(exePath)
		} else {
			dir = filepath.Dir(os.Args[0])
		}
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}
