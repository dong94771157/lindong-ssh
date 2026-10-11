//go:build windows

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestWriteUpdateScriptWindows(t *testing.T) {
	p, err := writeUpdateScript(`C:\测试\琳东SSH管理软件.exe`, `C:\测试\琳东SSH管理软件.exe.new`, 12345)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "琳东SSH管理软件.exe") ||
		!strings.Contains(content, "Move-Item") ||
		!strings.Contains(content, "12345") ||
		!bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) { // UTF-8 BOM
		t.Fatalf("script content invalid: %q", content[:80])
	}
}
