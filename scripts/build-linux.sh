#!/usr/bin/env bash
# Linux 构建脚本：产出 AppImage 与 .deb（在 Ubuntu 24.04 / GitHub Actions 上运行）
# 依赖：libgtk-3-dev libwebkit2gtk-4.1-dev build-essential pkg-config
set -euo pipefail
VERSION="${1:?用法: build-linux.sh <版本号，如 1.1.0>}"
cd "$(dirname "$0")/.."
mkdir -p dist build/appimage

# ---------------- 编译（ASCII 二进制名，中文名仅出现在桌面项里） ----------------
wails build -clean -platform linux/amd64 -o lindong-ssh
BIN="build/bin/lindong-ssh"
file "$BIN"

# ---------------- 组装 AppDir ----------------
APPDIR="build/appimage/AppDir"
rm -rf "$APPDIR"
mkdir -p "$APPDIR/usr/bin"
cp "$BIN" "$APPDIR/usr/bin/lindong-ssh"
cp build/appicon.png "$APPDIR/lindong-ssh.png"
chmod +x "$APPDIR/usr/bin/lindong-ssh"

cat > "$APPDIR/lindong-ssh.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=琳东SSH管理软件
Comment=安全高效的SSH连接管理软件
Exec=lindong-ssh
Icon=lindong-ssh
Categories=Network;System;TerminalEmulator;
Terminal=false
EOF

cat > "$APPDIR/AppRun" <<'EOF'
#!/bin/bash
HERE="$(dirname "$(readlink -f "$0")")"
exec "$HERE/usr/bin/lindong-ssh" "$@"
EOF
chmod +x "$APPDIR/AppRun"

# ---------------- 生成 AppImage ----------------
TOOL="build/appimage/appimagetool-x86_64.AppImage"
if [ ! -f "$TOOL" ]; then
  curl -fL --retry 3 -o "$TOOL" \
    https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage
  chmod +x "$TOOL"
fi
ARCH=x86_64 "$TOOL" --appimage-extract-and-run "$APPDIR" "dist/lindong-ssh-linux-x86_64.AppImage"
chmod +x "dist/lindong-ssh-linux-x86_64.AppImage"

# ---------------- 生成 .deb（nfpm 单文件版） ----------------
NFPM="build/appimage/nfpm"
if [ ! -x "$NFPM" ]; then
  curl -fL --retry 3 -o build/appimage/nfpm.tar.gz \
    "https://github.com/goreleaser/nfpm/releases/download/v2.44.1/nfpm_2.44.1_Linux_x86_64.tar.gz"
  tar -xzf build/appimage/nfpm.tar.gz -C build/appimage nfpm
fi

cat > build/appimage/nfpm.yaml <<EOF
name: lindong-ssh
arch: amd64
platform: linux
version: "$VERSION"
section: net
priority: optional
maintainer: "琳东 <support@ldnet.sryze.cc>"
description: 琳东SSH管理软件 - 安全高效的SSH连接管理软件
license: Proprietary
homepage: https://a.ldnet.sryze.cc
depends:
  - libc6
  - libgtk-3-0
  - libwebkit2gtk-4.1-0 | libwebkit2gtk-4.1-0t64
contents:
  - src: build/bin/lindong-ssh
    dst: /usr/bin/lindong-ssh
  - src: build/appimage/AppDir/lindong-ssh.desktop
    dst: /usr/share/applications/lindong-ssh.desktop
  - src: build/appicon.png
    dst: /usr/share/icons/hicolor/512x512/apps/lindong-ssh.png
EOF
"$NFPM" package -f build/appimage/nfpm.yaml -p deb -t dist/

echo "---- dist ----"
ls -lh dist/
