#!/usr/bin/env bash
# macOS 构建脚本：产出 Intel+Apple Silicon 通用 .dmg 与供应用内更新用的原始通用二进制
# 需在 macOS（Xcode Command Line Tools）上运行
set -euo pipefail
VERSION="${1:?用法: build-darwin.sh <版本号，如 1.1.0>}"
cd "$(dirname "$0")/.."
mkdir -p dist

# ---------------- 同步 Info.plist 版本号 ----------------
python3 - "$VERSION" <<'PY'
import re, sys
p = "build/darwin/Info.plist"
v = sys.argv[1]
s = open(p, encoding="utf-8").read()
s = re.sub(r"(<key>CFBundle(?:ShortVersionString|Version)</key>\s*<string>)[^<]+",
           lambda m: m.group(1) + v, s)
open(p, "w", encoding="utf-8").write(s)
print("Info.plist ->", v)
PY

# ---------------- 通用二进制（amd64 + arm64） ----------------
wails build -clean -platform darwin/universal -o lindong-ssh

APP="build/bin/琳东SSH管理软件.app"
mv build/bin/lindong-ssh.app "$APP"

# 应用内自更新下载的是包内原始可执行文件（ASCII 名称）
cp "$APP/Contents/MacOS/lindong-ssh" build/bin/lindong-ssh-darwin-universal
chmod +x build/bin/lindong-ssh-darwin-universal

# 临时签名（ad-hoc）：Apple Silicon 要求至少有一个签名；
# 未使用 Apple Developer 证书，用户首次打开需右键 -> 打开
codesign --force --deep -s - "$APP"

# ---------------- 制作 DMG ----------------
DMGROOT="build/dmgroot"
rm -rf "$DMGROOT"
mkdir -p "$DMGROOT"
cp -R "$APP" "$DMGROOT/"
ln -s /Applications "$DMGROOT/Applications"
hdiutil create -volname "琳东SSH管理软件" -srcfolder "$DMGROOT" -ov -format UDZO \
  "dist/lindong-ssh_${VERSION}_universal.dmg"

cp build/bin/lindong-ssh-darwin-universal dist/lindong-ssh-darwin-universal

echo "---- dist ----"
ls -lh dist/
