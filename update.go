package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ---------------- 在线更新配置 ----------------

// AppVersion 当前版本号（发布新版本时修改这里、wails.json 和 setup.iss）
const AppVersion = "1.1.0"

// UpdateManifestURL 更新清单地址（Cloudflare R2 桶 lindong-ssh，自定义域名 a.ldnet.sryze.cc）
// version.json 内容格式：
//
//	{
//	  "version":"1.1.0","url":"...windows 旧字段兼容...","notes":"...","force":false,
//	  "platforms":{
//	    "windows":{"url":"...","size":123,"sha256":"..."},
//	    "linux":  {"url":"...","size":123,"sha256":"..."},
//	    "darwin": {"url":"...","size":123,"sha256":"..."}
//	  }
//	}
//
// force 为 true 时弹窗不可关闭，用户必须更新
const UpdateManifestURL = "https://a.ldnet.sryze.cc/version.json"

// PlatformAsset 单个平台的安装包信息
type PlatformAsset struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type UpdateInfo struct {
	Version   string                   `json:"version"`
	URL       string                   `json:"url"`
	Notes     string                   `json:"notes"`
	Force     bool                     `json:"force"`
	Size      int64                    `json:"size"`
	SHA256    string                   `json:"sha256"`
	Platforms map[string]PlatformAsset `json:"platforms,omitempty"`
}

// resolveForCurrentPlatform 按当前系统选择安装包，并把结果写回顶层字段，
// 前端只读取 url/size/sha256 即可，无需关心平台分支
func (u *UpdateInfo) resolveForCurrentPlatform() {
	if len(u.Platforms) == 0 {
		return
	}
	if a, ok := u.Platforms[runtime.GOOS]; ok && a.URL != "" {
		u.URL = a.URL
		u.Size = a.Size
		u.SHA256 = a.SHA256
	}
}

// ---------------- 版本比较 ----------------

// newerVersion 判断 remote 版本是否比 current 新（按 . 分段数值比较）
func newerVersion(remote, current string) bool {
	r := strings.Split(strings.TrimPrefix(strings.TrimSpace(remote), "v"), ".")
	c := strings.Split(strings.TrimPrefix(strings.TrimSpace(current), "v"), ".")
	n := len(r)
	if len(c) > n {
		n = len(c)
	}
	for i := 0; i < n; i++ {
		var rv, cv int
		if i < len(r) {
			rv, _ = strconv.Atoi(r[i])
		}
		if i < len(c) {
			cv, _ = strconv.Atoi(c[i])
		}
		if rv != cv {
			return rv > cv
		}
	}
	return false
}

// ---------------- 检查更新 ----------------

// GetVersion 返回当前版本号（供前端显示）
func (a *App) GetVersion() string {
	return AppVersion
}

// GetPlatform 返回当前操作系统标识（windows / linux / darwin）
func (a *App) GetPlatform() string {
	return runtime.GOOS
}

// ChangelogURL 更新说明地址
const ChangelogURL = "https://a.ldnet.sryze.cc/changelog.md"

// GetChangelog 从服务端拉取更新说明（由 Go 端请求，避免 WebView 跨域限制）
func (a *App) GetChangelog() (string, error) {
	req, err := http.NewRequest("GET", ChangelogURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("无法连接更新服务器：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("更新服务器返回 %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (a *App) httpClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// CheckUpdate 检查更新，无新版本时返回 nil
func (a *App) CheckUpdate() (*UpdateInfo, error) {
	return checkUpdateFrom(a.httpClient(), UpdateManifestURL, AppVersion)
}

func checkUpdateFrom(client *http.Client, manifestURL, current string) (*UpdateInfo, error) {
	if manifestURL == "" {
		return nil, nil // 未配置更新地址，静默跳过
	}
	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "LinDongSSH/"+current+" ("+runtime.GOOS+")")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接更新服务器")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("更新服务器返回 %d", resp.StatusCode)
	}
	var info UpdateInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return nil, errors.New("更新信息格式错误")
	}
	info.resolveForCurrentPlatform()
	if info.Version == "" || info.URL == "" {
		return nil, errors.New("更新信息不完整")
	}
	if !newerVersion(info.Version, current) {
		return nil, nil
	}
	return &info, nil
}

// ---------------- 下载并应用更新 ----------------

// ApplyUpdate 下载新版本并启动更新脚本，成功后前端应立即退出程序
// expectSize > 0 或 expectSHA256 非空时对下载结果做完整性校验
func (a *App) ApplyUpdate(rawURL string, expectSize int64, expectSHA256 string) error {
	exeTarget, err := runningExePath()
	if err != nil {
		return err
	}
	newPath := exeTarget + ".new"
	os.Remove(newPath) // 清理历史残留

	if err := a.downloadFile(rawURL, newPath, expectSize, expectSHA256); err != nil {
		os.Remove(newPath)
		return err
	}

	scriptPath, err := writeUpdateScript(exeTarget, newPath, os.Getpid())
	if err != nil {
		os.Remove(newPath)
		return err
	}
	return launchUpdater(scriptPath)
}

// withCacheBuster 给 URL 追加唯一查询参数，强制 CDN/代理回源，避免命中边缘脏缓存
func withCacheBuster(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set("_", strconv.FormatInt(time.Now().UnixNano(), 10))
	u.RawQuery = q.Encode()
	return u.String()
}

// downloadFile 下载文件并推送进度事件 update:progress (0-100)
// 自带 3 次重试、强制回源、Content-Length / size / sha256 校验
func (a *App) downloadFile(rawURL, dest string, expectSize int64, expectSHA256 string) error {
	expectSHA256 = strings.ToLower(strings.TrimSpace(expectSHA256))
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			a.emitEvent("update:progress", 0)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		lastErr = a.downloadOnce(withCacheBuster(rawURL), dest, expectSize, expectSHA256)
		if lastErr == nil {
			return nil
		}
		os.Remove(dest)
	}
	return lastErr
}

func (a *App) downloadOnce(freshURL, dest string, expectSize int64, expectSHA256 string) error {
	req, err := http.NewRequest("GET", freshURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "LinDongSSH/"+AppVersion+" ("+runtime.GOOS+")")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	// 下载不限制总超时（文件可能较大），但限制建连与响应头时间
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接下载服务器，请检查网络后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("下载服务器返回 %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("写入文件失败: %v", err)
	}

	total := resp.ContentLength
	if expectSize > 0 && total > 0 && total != expectSize {
		out.Close()
		return fmt.Errorf("下载文件大小异常（%d 字节，应为 %d）", total, expectSize)
	}
	if total <= 0 {
		total = expectSize // 服务器未给长度时用清单长度驱动进度条
	}

	h := sha256.New()
	var downloaded int64
	lastPct := -1
	buf := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return fmt.Errorf("写入文件失败: %v", werr)
			}
			h.Write(buf[:n])
			downloaded += int64(n)
			if total > 0 {
				pct := int(downloaded * 100 / total)
				if pct > 100 {
					pct = 100
				}
				if pct != lastPct && pct%2 == 0 {
					lastPct = pct
					a.emitEvent("update:progress", pct)
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return fmt.Errorf("下载中断（网络不稳定），将自动重试")
		}
	}
	if cerr := out.Close(); cerr != nil {
		return fmt.Errorf("保存文件失败: %v", cerr)
	}

	if expectSize > 0 && downloaded != expectSize {
		return fmt.Errorf("下载不完整（收到 %d / 应为 %d 字节），将自动重试", downloaded, expectSize)
	}
	if expectSHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if got != expectSHA256 {
			return fmt.Errorf("安装包校验失败（文件已损坏），将自动重试")
		}
	}
	a.emitEvent("update:progress", 100)
	return nil
}

// emitEvent 安全地推送事件（无窗口环境如单元测试时跳过）
func (a *App) emitEvent(name string, data interface{}) {
	if a.ctx != nil {
		wailsRuntime.EventsEmit(a.ctx, name, data)
	}
}

// updateLogPath 各平台更新日志路径
func updateLogPath() string {
	return filepath.Join(os.TempDir(), "lindong-ssh-update.log")
}
