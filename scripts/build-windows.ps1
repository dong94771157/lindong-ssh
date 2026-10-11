# Windows 构建脚本：产出与在线更新同名的 lindong-ssh.exe
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
New-Item -ItemType Directory -Force -Path dist | Out-Null
wails build -clean
Copy-Item "build\bin\琳东SSH管理软件.exe" "dist\lindong-ssh.exe" -Force
Write-Host "---- dist ----"
Get-ChildItem dist | Format-Table Name, Length
