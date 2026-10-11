package main

import "path/filepath"

// dataFile 返回用户数据文件（servers.json / ai.json）的完整路径
func dataFile(name string) string {
	return filepath.Join(dataDir(), name)
}
