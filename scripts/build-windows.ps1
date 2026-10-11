# Windows 构建脚本：产出与在线更新同名的 lindong-ssh.exe
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
New-Item -ItemType Directory -Force -Path dist | Out-Null
wails build -clean
# 用通配符匹配产物，避免 ps1 中文文件名编码问题
$exe = Get-ChildItem "build\bin\*.exe" | Select-Object -First 1
if (-not $exe) { throw "build\bin 下未找到 exe 产物" }
Copy-Item $exe.FullName "dist\lindong-ssh.exe" -Force
Write-Host "---- dist ----"
Get-ChildItem dist | Format-Table Name, Length
