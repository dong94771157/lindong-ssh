//go:build !windows

package main

import (
	"os"
	"strings"
	"testing"
)

func TestWriteUpdateScriptUnix(t *testing.T) {
	p, err := writeUpdateScript(`/tmp/测试目录/lindong-ssh`, `/tmp/测试目录/lindong-ssh.new`, 12345)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	info, _ := os.Stat(p)
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("update script must be executable")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "测试目录/lindong-ssh") ||
		!strings.Contains(content, "mv -f") ||
		!strings.Contains(content, "chmod") ||
		!strings.Contains(content, "12345") {
		t.Fatalf("script content invalid: %q", content[:80])
	}
}
