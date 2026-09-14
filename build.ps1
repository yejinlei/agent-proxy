param([string]$Version = "v0.2.107")
$ErrorActionPreference = "Stop"
Set-Location "F:\src\agent-proxy"
# 用法: pwsh build.ps1 -Version v0.2.107   (或旧版 PowerShell: powershell build.ps1 -Version v0.2.107)
$VERSION = $Version
$OUTDIR = "build\$VERSION"
New-Item -ItemType Directory -Force -Path $OUTDIR | Out-Null

$combos = @(
    @{ GOOS="windows"; GOARCH="amd64"; OUT="$OUTDIR\agent-proxy_windows_amd64.exe" }
    @{ GOOS="windows"; GOARCH="arm64"; OUT="$OUTDIR\agent-proxy_windows_arm64.exe" }
    @{ GOOS="linux"; GOARCH="amd64"; OUT="$OUTDIR\agent-proxy_linux_amd64" }
    @{ GOOS="linux"; GOARCH="arm64"; OUT="$OUTDIR\agent-proxy_linux_arm64" }
    @{ GOOS="darwin"; GOARCH="amd64"; OUT="$OUTDIR\agent-proxy_darwin_amd64" }
    @{ GOOS="darwin"; GOARCH="arm64"; OUT="$OUTDIR\agent-proxy_darwin_arm64" }
)

foreach ($c in $combos) {
    $env:GOOS = $c['GOOS']
    $env:GOARCH = $c['GOARCH']
    Write-Output "Building $c['OUT']..."
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o $c['OUT'] .
    Write-Output "  done"
}

Set-Location $OUTDIR
# sha256sums.txt 必须是「双空格 + 裸文件名 + LF + 无 BOM」，这是 GNU sha256sum 的格式约定，
# 与 build/v0.2.138/、build/v0.2.139/、dist/ 的既有产物一致。
# 两个坑，都会让 Linux/macOS 上 `sha256sum -c` 全部失败：
#   1. 换行符 —— `Out-File` 和 `Set-Content` 都写 CRLF（PowerShell 用环境换行符，
#      与 -Encoding 无关），sha256sum 把 \r 当成文件名的一部分。
#   2. BOM / 全路径 —— `Out-File -Encoding utf8` 带 BOM；`$_.Path` 带全路径，
#      校验方得先 cd 到同目录才能过。
$sumLines = Get-FileHash agent-proxy_* -Algorithm SHA256 | ForEach-Object {
    "{0}  {1}" -f $_.Hash.ToLower(), $_.Path.Split([IO.Path]::DirectorySeparatorChar)[-1]
}
# WriteAllText + 手工拼 `\n` 才是真 LF；ASCII 编码天然无 BOM（哈希与文件名都是 ASCII）
[System.IO.File]::WriteAllText(
    (Join-Path $PWD 'sha256sums.txt'),
    (($sumLines -join "`n") + "`n"),
    [System.Text.Encoding]::ASCII)
Write-Output "`nAll builds complete. Output: $PWD"
Get-Content sha256sums.txt