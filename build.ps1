$ErrorActionPreference = "Stop"
Set-Location "F:\src\agent-proxy"
$VERSION = "v0.2.99"

$combos = @(
    @{ GOOS="windows"; GOARCH="amd64"; OUT="dist\agent-proxy_windows_amd64.exe" }
    @{ GOOS="windows"; GOARCH="arm64"; OUT="dist\agent-proxy_windows_arm64.exe" }
    @{ GOOS="linux"; GOARCH="amd64"; OUT="dist\agent-proxy_linux_amd64" }
    @{ GOOS="linux"; GOARCH="arm64"; OUT="dist\agent-proxy_linux_arm64" }
    @{ GOOS="darwin"; GOARCH="amd64"; OUT="dist\agent-proxy_darwin_amd64" }
    @{ GOOS="darwin"; GOARCH="arm64"; OUT="dist\agent-proxy_darwin_arm64" }
)

foreach ($c in $combos) {
    $goos = $c['GOOS']
    $goarch = $c['GOARCH']
    $out = $c['OUT']
    $env:GOOS = $goos
    $env:GOARCH = $goarch
    Write-Output "Building $out..."
    go build -ldflags "-X main.version=$VERSION" -o $out .
    Write-Output "  done"
}

Write-Output "All builds complete."