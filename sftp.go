package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// dialSSH 使用服务器配置建立 SSH 连接（终端与文件上传共用）
func (a *App) dialSSH(sv Server) (*ssh.Client, error) {
	auths := []ssh.AuthMethod{}
	if sv.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(sv.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("私钥解析失败: %v", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if sv.Password != "" {
		auths = append(auths, ssh.Password(sv.Password), ssh.KeyboardInteractive(
			func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = sv.Password
				}
				return answers, nil
			}))
	}
	if len(auths) == 0 {
		return nil, errors.New("请提供密码或私钥")
	}

	cfg := &ssh.ClientConfig{
		User:            sv.Username,
		Auth:            auths,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         12 * time.Second,
		ClientVersion:   "SSH-2.0-LinDongSSH",
	}
	port := sv.Port
	if port <= 0 {
		port = 22
	}
	addr := net.JoinHostPort(sv.Host, fmt.Sprintf("%d", port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("连接失败: %v", err)
	}
	return client, nil
}

// ---------------- SFTP 文件上传 ----------------

type UploadRequest struct {
	ServerID   string `json:"serverId"`
	LocalPath  string `json:"localPath"`
	RemotePath string `json:"remotePath"` // 远程完整文件路径
}

// UploadProgress 上传进度事件载荷
type UploadProgress struct {
	Name string `json:"name"`
	Pct  int    `json:"pct"`
	Done bool   `json:"done"`
	Err  string `json:"err"`
}

// UploadFile 通过 SFTP 上传单个文件，进度经 upload:progress 事件推送
func (a *App) UploadFile(req UploadRequest) error {
	if req.ServerID == "" || req.LocalPath == "" || req.RemotePath == "" {
		return errors.New("参数不完整")
	}
	sv, ok := a.store.get(req.ServerID)
	if !ok {
		return errors.New("服务器不存在")
	}

	f, err := os.Open(req.LocalPath)
	if err != nil {
		return fmt.Errorf("打开本地文件失败: %v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	client, err := a.dialSSH(sv)
	if err != nil {
		return err
	}
	defer client.Close()

	sftpc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("初始化 SFTP 失败: %v", err)
	}
	defer sftpc.Close()

	// 逐级创建远程目录
	remote := filepathSlash(req.RemotePath)
	dir := path.Dir(remote)
	if dir != "." && dir != "/" {
		_ = sftpc.MkdirAll(dir)
	}

	out, err := sftpc.Create(remote)
	if err != nil {
		return fmt.Errorf("创建远程文件失败: %v（如目标目录无权限请更换路径）", err)
	}
	defer out.Close()

	name := path.Base(remote)
	total := info.Size()
	var written int64
	lastPct := -1
	buf := make([]byte, 64*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			w, werr := out.Write(buf[:n])
			if werr != nil {
				wailsRuntime.EventsEmit(a.ctx, "upload:progress", UploadProgress{Name: name, Err: werr.Error()})
				return fmt.Errorf("写入远程文件失败: %v", werr)
			}
			written += int64(w)
			if total > 0 {
				pct := int(written * 100 / total)
				if pct != lastPct && pct%2 == 0 {
					lastPct = pct
					wailsRuntime.EventsEmit(a.ctx, "upload:progress", UploadProgress{Name: name, Pct: pct})
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			wailsRuntime.EventsEmit(a.ctx, "upload:progress", UploadProgress{Name: name, Err: rerr.Error()})
			return rerr
		}
	}
	wailsRuntime.EventsEmit(a.ctx, "upload:progress", UploadProgress{Name: name, Pct: 100, Done: true})
	return nil
}

func filepathSlash(p string) string {
	// 远程路径统一为正斜杠
	out := make([]rune, 0, len(p))
	for _, c := range p {
		if c == '\\' {
			out = append(out, '/')
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}
