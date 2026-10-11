//go:build windows

package main

import (
	"os"
	"path/filepath"
)

// dataDir Windows：保持数据文件（servers.json / ai.json）位于程序同目录，
// 兼容绿色版与安装版老用户的既有数据
func dataDir() string {
	exePath, err := os.Executable()
	if err != nil {
		exePath = os.Args[0]
	}
	return filepath.Dir(exePath)
}
