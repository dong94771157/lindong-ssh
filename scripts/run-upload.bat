@echo off
start "R2 Upload" powershell.exe -NoExit -ExecutionPolicy Bypass -File "%~dp0upload-r2.ps1"
