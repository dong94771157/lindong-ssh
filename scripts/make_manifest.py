#!/usr/bin/env python3
"""根据 dist/ 下的产物生成三端统一的 version.json。
用法：VERSION=1.1.0 python3 scripts/make_manifest.py
"""
import hashlib
import json
import os
import re
import sys

BASE = "https://a.ldnet.sryze.cc"
VERSION = os.environ.get("VERSION", "").strip()
if not VERSION:
    sys.exit("缺少 VERSION 环境变量")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "dist")

NAMES = {
    "windows": "lindong-ssh.exe",
    "linux_appimage": "lindong-ssh-linux-x86_64.AppImage",
    "linux_deb": f"lindong-ssh_{VERSION}_amd64.deb",
    "darwin_bin": "lindong-ssh-darwin-universal",
    "darwin_dmg": f"lindong-ssh_{VERSION}_universal.dmg",
}


def asset(filename):
    path = os.path.join(DIST, filename)
    if not os.path.exists(path):
        sys.exit(f"缺少产物：{filename}")
    with open(path, "rb") as f:
        digest = hashlib.sha256(f.read()).hexdigest()
    return {
        "url": f"{BASE}/{filename}",
        "size": os.path.getsize(path),
        "sha256": digest,
    }


def extract_notes():
    """从 changelog.md 取当前版本段（第一个 vX.Y.Z 标题到下一个同级标题之间）。"""
    path = os.path.join(ROOT, "release", "changelog.md")
    if not os.path.exists(path):
        return f"琳东SSH管理软件 v{VERSION}"
    text = open(path, encoding="utf-8").read()
    m = re.search(r"^v" + re.escape(VERSION) + r"[^\n]*\n(.*?)(?=^v\d|\Z)",
                  text, re.M | re.S)
    notes = m.group(1).strip() if m else ""
    # 清单 notes 用单行简洁文本，去掉编号换行
    notes = re.sub(r"\s*\n\s*", "\n", notes)
    return notes


win = asset(NAMES["windows"])
# deb/dmg 仅作为手动下载产物，确认其存在
for key in ("linux_deb", "darwin_dmg"):
    asset(NAMES[key])
manifest = {
    "version": VERSION,
    "notes": extract_notes(),
    "force": os.environ.get("FORCE", "true").lower() == "true",
    # 顶层字段为 windows，兼容 1.0.x 老客户端
    "url": win["url"],
    "size": win["size"],
    "sha256": win["sha256"],
    "platforms": {
        "windows": win,
        "linux": asset(NAMES["linux_appimage"]),
        "darwin": asset(NAMES["darwin_bin"]),
    },
    "downloads": {
        "deb": f"{BASE}/lindong-ssh_amd64.deb",
        "dmg": f"{BASE}/lindong-ssh_universal.dmg",
    },
}

out = os.path.join(DIST, "version.json")
with open(out, "w", encoding="utf-8") as f:
    json.dump(manifest, f, ensure_ascii=False, indent=2)
    f.write("\n")
print(json.dumps(manifest, ensure_ascii=False, indent=2))
