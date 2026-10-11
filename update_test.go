package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		remote, current string
		want            bool
	}{
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.9", true},
		{"2.0", "1.9.9", true},
		{"v1.0.1", "1.0.0", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"1.0", "1.0.1", false},
		{"1.0.1", "1.0", true},
	}
	for _, c := range cases {
		if got := newerVersion(c.remote, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.remote, c.current, got, c.want)
		}
	}
}

func TestCheckUpdateFrom(t *testing.T) {
	manifest := `{"version":"1.0.1","url":"http://example.com/app.exe","notes":"修复若干问题"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, manifest)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	// 有新版本
	info, err := checkUpdateFrom(client, srv.URL, "1.0.0")
	if err != nil || info == nil {
		t.Fatalf("expected update info, got info=%v err=%v", info, err)
	}
	if info.Version != "1.0.1" || info.Notes == "" {
		t.Fatalf("unexpected info: %+v", info)
	}

	// 无新版本
	info, err = checkUpdateFrom(client, srv.URL, "1.0.1")
	if err != nil || info != nil {
		t.Fatalf("expected nil info, got info=%v err=%v", info, err)
	}

	// 空地址静默跳过
	info, err = checkUpdateFrom(client, "", "1.0.0")
	if err != nil || info != nil {
		t.Fatalf("expected nil for empty url, got info=%v err=%v", info, err)
	}
}

func TestCheckUpdateFromBadServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	_, err := checkUpdateFrom(&http.Client{Timeout: 3 * time.Second}, srv.URL, "1.0.0")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestDownloadFile(t *testing.T) {
	payload := make([]byte, 300*1024) // 300KB 触发多次进度
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "app.exe.new")
	a := NewApp() // ctx 为 nil，emitEvent 应安全跳过
	goodSHA := hex.EncodeToString(sha256New(payload))
	if err := a.downloadFile(srv.URL, dest, int64(len(payload)), goodSHA); err != nil {
		t.Fatalf("downloadFile failed: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || len(data) != len(payload) {
		t.Fatalf("downloaded size mismatch: %v, len=%d", err, len(data))
	}
	for i := 0; i < len(payload); i += 4096 {
		if data[i] != payload[i] {
			t.Fatal("content mismatch")
		}
	}

	// 错误的 SHA256 必须导致失败（重试耗尽）
	badDest := filepath.Join(t.TempDir(), "bad.exe.new")
	if err := a.downloadFile(srv.URL, badDest, 0, strings.Repeat("0", 64)); err == nil {
		t.Fatal("expected sha256 verification failure")
	}
}

func sha256New(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
