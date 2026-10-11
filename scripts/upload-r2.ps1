# 琳东SSH管理软件 —— R2 一键发布脚本（C# SigV4 内核，PowerShell 5.1/7 通用）
$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$AcctId = "b0706cf2749dc0022f4ea75e684cfcd4"
$Bucket = "lindong-ssh"
$Domain = "a.ldnet.sryze.cc"
$Root   = Split-Path -Parent $PSScriptRoot
$Files  = @(
    @{ Local = Join-Path $Root "release\version.json";   Key = "version.json";   Ct = "application/json; charset=utf-8"; CC = "no-cache" },
    @{ Local = Join-Path $Root "release\changelog.md";   Key = "changelog.md";   Ct = "text/markdown; charset=utf-8"; CC = "no-cache" },
    @{ Local = Join-Path $Root "release\lindong-ssh.exe"; Key = "lindong-ssh.exe"; Ct = "application/vnd.microsoft.portable-executable"; CC = "no-cache" },
    # 以下为 Linux/macOS 产物（由 GitHub Actions 产出后放入 release 目录；不存在则跳过）
    @{ Local = Join-Path $Root "release\lindong-ssh-linux-x86_64.AppImage"; Key = "lindong-ssh-linux-x86_64.AppImage"; Ct = "application/octet-stream"; CC = "no-cache"; Opt = $true },
    @{ Local = Join-Path $Root "release\lindong-ssh_amd64.deb"; Key = "lindong-ssh_amd64.deb"; Ct = "application/vnd.debian.binary-package"; CC = "no-cache"; Opt = $true },
    @{ Local = Join-Path $Root "release\lindong-ssh-darwin-universal"; Key = "lindong-ssh-darwin-universal"; Ct = "application/octet-stream"; CC = "no-cache"; Opt = $true },
    @{ Local = Join-Path $Root "release\lindong-ssh_universal.dmg"; Key = "lindong-ssh_universal.dmg"; Ct = "application/x-apple-diskimage"; CC = "no-cache"; Opt = $true }
)

# ---------------- C# SigV4 签名内核（已通过 AWS 官方测试向量验证） ----------------
Add-Type -Language CSharp -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Security.Cryptography;
using System.Text;
public class R2Sig {
    static string Hex(byte[] b){ return BitConverter.ToString(b).Replace("-","").ToLowerInvariant(); }
    static byte[] Hmac(byte[] k,string d){ using(var h=new HMACSHA256(k)) return h.ComputeHash(Encoding.UTF8.GetBytes(d)); }
    static byte[] Sha(byte[] b){ using(var q=SHA256.Create()) return q.ComputeHash(b); }
    static readonly string Empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";

    static Dictionary<string,string> Sign(string method, string canonUri, string query,
        string accessKey, string secret, string host, SortedDictionary<string,string> hdrs, byte[] payload){
        var now = DateTime.UtcNow;
        string amz = now.ToString("yyyyMMddTHHmmssZ"), date = now.ToString("yyyyMMdd");
        hdrs["host"] = host;
        hdrs["x-amz-date"] = amz;
        hdrs["x-amz-content-sha256"] = payload==null ? Empty : Hex(Sha(payload));
        var ch = new StringBuilder();
        foreach(var kv in hdrs){ ch.Append(kv.Key).Append(':').Append(kv.Value.Trim()).Append('\n'); }
        string signed = string.Join(";", hdrs.Keys);
        string creq = string.Join("\n", method, canonUri, query, ch.ToString(), signed, hdrs["x-amz-content-sha256"]);
        string scope = date + "/auto/s3/aws4_request";
        string sts = "AWS4-HMAC-SHA256\n" + amz + "\n" + scope + "\n" + Hex(Sha(Encoding.UTF8.GetBytes(creq)));
        var k = Hmac(Encoding.UTF8.GetBytes("AWS4" + secret), date);
        k = Hmac(k, "auto"); k = Hmac(k, "s3"); k = Hmac(k, "aws4_request");
        string sig = Hex(Hmac(k, sts));
        var r = new Dictionary<string,string>();
        r["x-amz-date"] = amz;
        r["x-amz-content-sha256"] = hdrs["x-amz-content-sha256"];
        r["Authorization"] = "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig;
        return r;
    }
    public static Dictionary<string,string> SignPut(string acct, string bucket, string key,
        string accessKey, string secret, string contentType, string cacheControl, byte[] data){
        var h = new SortedDictionary<string,string>();
        if(!string.IsNullOrEmpty(contentType)) h["content-type"] = contentType;
        if(!string.IsNullOrEmpty(cacheControl)) h["cache-control"] = cacheControl;
        return Sign("PUT", "/" + bucket + "/" + key, "", accessKey, secret, acct + ".r2.cloudflarestorage.com", h, data);
    }
    public static Dictionary<string,string> SignGet(string acct, string bucket, string query, string accessKey, string secret){
        var h = new SortedDictionary<string,string>();
        return Sign("GET", "/" + bucket, query, accessKey, secret, acct + ".r2.cloudflarestorage.com", h, null);
    }
}
'@

function Read-Plain($p){ Write-Host $p -ForegroundColor Cyan -NoNewline; return (Read-Host).Trim() }

Write-Host "===== 琳东SSH R2 发布 =====" -ForegroundColor White
Write-Host "密钥来源：Cloudflare 控制台 → R2 → 管理 R2 API 令牌 → 创建账户 API 令牌（管理员读和写）`n" -ForegroundColor DarkGray
$AccessKey = (Read-Plain "请输入 Access Key ID（32位）: ") -replace '\s',''
Write-Host "请输入 Secret Access Key（64位，明文便于核对）:" -ForegroundColor Cyan
$SecretKey = ((Read-Host).Trim()) -replace '\s',''
Write-Host ("  长度: AccessKey=" + $AccessKey.Length + "  SecretKey=" + $SecretKey.Length) -ForegroundColor DarkGray
if ($AccessKey -notmatch '^[A-Za-z0-9]{32}$' -or $SecretKey -notmatch '^[A-Za-z0-9]{64}$') {
    Write-Host "密钥格式不正确（应为 32 / 64 位字母数字）。" -ForegroundColor Red; exit 1
}

function ErrBody($e){
    if ($e.ErrorDetails -and $e.ErrorDetails.Message) { return $e.ErrorDetails.Message }
    try { if ($e.Exception.Response) { $sr = New-Object IO.StreamReader($e.Exception.Response.GetResponseStream()); return $sr.ReadToEnd() } } catch {}
    return $e.Exception.Message
}

# ---------------- 鉴权预检 ----------------
Write-Host "`n[预检] 验证密钥对 ..." -ForegroundColor Yellow
try {
    $gh = [R2Sig]::SignGet($AcctId, $Bucket, "list-type=2&max-keys=1", $AccessKey, $SecretKey)
    $null = Invoke-WebRequest "https://$AcctId.r2.cloudflarestorage.com/$Bucket`?list-type=2&max-keys=1" -Headers $gh -TimeoutSec 30 -UseBasicParsing
    Write-Host "  密钥对有效且有访问权限" -ForegroundColor Green
} catch {
    $d = ErrBody $_
    if ($d -match "SignatureDoesNotMatch") { Write-Host "  Secret 与 Access Key ID 不匹配（两者必须来自同一次令牌创建）。" -ForegroundColor Red; exit 1 }
    elseif ($d -match "InvalidAccessKeyId") { Write-Host "  Access Key ID 无效。" -ForegroundColor Red; exit 1 }
    elseif ($d -match "AccessDenied") { Write-Host "  密钥有效但权限不足，请改用管理员读和写令牌。" -ForegroundColor Red; exit 1 }
    else { Write-Host "  预检异常：$d" -ForegroundColor Yellow }
}

# ---------------- 上传 ----------------
Write-Host "`n[1/2] 上传 ..." -ForegroundColor Yellow
foreach ($f in $Files) {
    if (-not (Test-Path $f.Local)) {
        if ($f.Opt) { Write-Host "  跳过（不存在）：$($f.Key)" -ForegroundColor DarkGray; continue }
        Write-Host "  缺少文件：$($f.Local)" -ForegroundColor Red; exit 1
    }
    $data = [IO.File]::ReadAllBytes($f.Local)
    $sizeMB = [math]::Round($data.Length / 1MB, 2)
    Write-Host "  $($f.Key) ($sizeMB MB) ..." -NoNewline
    $h = [R2Sig]::SignPut($AcctId, $Bucket, $f.Key, $AccessKey, $SecretKey, $f.Ct, $f.CC, $data)
    if ($f.Ct) { $h["Content-Type"] = $f.Ct }
    if ($f.CC) { $h["Cache-Control"] = $f.CC }
    try {
        $null = Invoke-WebRequest -Method Put -Uri "https://$AcctId.r2.cloudflarestorage.com/$Bucket/$($f.Key)" -Headers $h -Body $data -TimeoutSec 600 -UseBasicParsing
        Write-Host " 完成" -ForegroundColor Green
    } catch { Write-Host " 失败：$(ErrBody $_)" -ForegroundColor Red; exit 1 }
}

# ---------------- 验证 ----------------
Write-Host "`n[2/2] 验证 https://$Domain/version.json ..." -ForegroundColor Yellow
for ($i=1; $i -le 6; $i++) {
    try {
        Start-Sleep -Seconds 3
        $v = Invoke-WebRequest "https://$Domain/version.json" -TimeoutSec 15 -UseBasicParsing -Headers @{ "Cache-Control"="no-cache" }
        if ($v.StatusCode -eq 200 -and $v.Content -match '"version"') {
            Write-Host "  线上清单：$($v.Content.Trim())" -ForegroundColor Green; Write-Host "`n发布成功。" -ForegroundColor Cyan; exit 0
        }
    } catch { Write-Host "  等待生效($i)..." -ForegroundColor DarkGray; Start-Sleep 8 }
}
Write-Host "  上传完成但暂未验证到公网访问，稍后手动打开 https://$Domain/version.json" -ForegroundColor Yellow
