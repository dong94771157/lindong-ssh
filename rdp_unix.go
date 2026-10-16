//go:build !windows

package main

import "errors"

// ConnectRDP 远程桌面功能仅 Windows 客户端提供
func (a *App) ConnectRDP(serverID string) error {
	return errors.New("远程桌面功能仅支持 Windows 客户端")
}
