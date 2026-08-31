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
Get-FileHash agent-proxy_* -Algorithm SHA256 | ForEach-Object { "{0}  {1}" -f $_.Hash.ToLower(), $_.Path } | Out-File -Encoding utf8 sha256sums.txt
Write-Output "`nAll builds complete. Output: $PWD"
Get-Content sha256sums.txt