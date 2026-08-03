#!/usr/bin/env python3
"""Create GitHub Release + upload binary for agent-proxy.
Usage:
  GH_TOKEN=<your_token> python scripts/publish_release.py
"""

import json, base64, hashlib, os, sys, urllib.request, urllib.error

REPO = "yejinlei/agent-proxy"
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
PROJECT_DIR = os.path.normpath(os.path.join(SCRIPT_DIR, ".."))
BINARY = os.path.join(PROJECT_DIR, "agent-proxy.exe")
TAG = "v0.1.0"

TOKEN = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")

def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(8192), b""):
            h.update(chunk)
    return h.hexdigest()

def gh_api(method, url, body=None):
    """Generic GitHub API call."""
    if method == "PUT":
        m = "PUT"
    elif body:
        m = "POST"
    else:
        m = "GET"
    data = json.dumps(body).encode() if body else None
    headers = {
        "Authorization": f"token {TOKEN}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    if data:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=m)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        print(f"HTTP {e.code}: {e.read().decode()}", file=sys.stderr)
        sys.exit(1)

def upload_asset(url, path):
    """Upload release asset using multipart form-data."""
    content = open(path, "rb").read()
    fname = os.path.basename(path)
    boundary = "----WebKitFormBoundary7MA4YWxkTrZu0gW"

    # Build multipart body
    body = b""
    body += f"--{boundary}\r\n".encode()
    body += b'Content-Disposition: form-data; name="file"; filename="'
    body += fname.encode()
    body += b'"\r\n'
    body += b"Content-Type: application/octet-stream\r\n\r\n"
    body += content
    body += b"\r\n"
    body += f"--{boundary}--\r\n".encode()

    print(f"  Uploading {fname} ({len(content):,} bytes / {len(content)//1024//1024} MB)...")
    headers = {
        "Authorization": f"token {TOKEN}",
        "Content-Type": f"multipart/form-data; boundary={boundary}",
        "Content-Length": str(len(body)),
    }
    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        print(f"  Upload failed HTTP {e.code}: {e.read().decode()}", file=sys.stderr)
        sys.exit(1)

def main():
    if not TOKEN:
        print("Set GH_TOKEN or GITHUB_TOKEN env var first.", file=sys.stderr)
        print("Get token: https://github.com/settings/tokens", file=sys.stderr)
        print("  Minimum scope: repo", file=sys.stderr)
        sys.exit(1)

    if not os.path.exists(BINARY):
        print(f"Binary not found: {BINARY}")
        print("Run: go build -o agent-proxy.exe ./cmd/server")
        sys.exit(1)

    checksum = sha256(BINARY)
    size = os.path.getsize(BINARY)
    print(f"Binary: {BINARY}")
    print(f"Size:   {size:,} bytes ({size//1024//1024} MB)")
    print(f"SHA256: {checksum}")
    print()

    # Step 1: Create release
    print(f"Creating release {TAG}...")
    html_url = f"https://github.com/{REPO}/releases/tag/{TAG}"
    download_table = f"""| 文件 | 平台 | 架构 | 大小 |
|------|------|------|------|
| agent-proxy.exe | Windows | amd64 | {size//1024} KB |"""

    release_body = {
        "tag_name": TAG,
        "target_commitish": "master",
        "name": f"agent-proxy {TAG}",
        "body": f"""## agent-proxy {TAG} — 4×4 全协议互转 AI 网关

### ✨ 核心功能

**4×4 协议互转矩阵**：OpenAI Compatible、Anthropic Messages、Google Gemini、OpenAI Responses **任意入站协议可转换为任意出站协议**。

| 入站 ↓ \\ 出站 → | OpenAI Compatible | Anthropic Messages | Google Gemini | OpenAI Responses |
|----------------|------------------|-------------------|---------------|-----------------|
| OpenAI Compatible `/v1/chat/completions` | ✅ 透传 | ✅ 双向翻译 | ✅ 双向翻译 | ✅ 双向翻译 |
| Anthropic Messages `/v1/messages` | ✅ 双向翻译 | ✅ 透传 | ✅ 双向翻译 | ✅ 双向翻译 |
| Google Gemini `/v1/models/{{model}}:generateContent` | ✅ 双向翻译 | ✅ 双向翻译 | ✅ 透传 | ✅ 双向翻译 |
| OpenAI Responses `/v1/responses` | ✅ 双向翻译 | ✅ 双向翻译 | ✅ 双向翻译 | ✅ 透传 |

翻译链路（N 协议只需 N 翻译器，而非 N×N）：
```
入站协议 TranslateRequest → InternalRequest → 路由 Provider
→ TranslateToProvider → Provider 调用
→ TranslateFromProvider → InternalResponse
→ 入站协议 TranslateResponse → 出站响应
```

### 🚀 两种运行模式

| 模式 | 命令 | 适用场景 |
|------|------|---------|
| **快速模式** | `agent-proxy run --mode simple --host 0.0.0.0 --port 8080 --db 1` | 单一端点、快速试用 |
| **复杂模式** | `agent-proxy run --mode complex --conf config.json` | 生产环境、多厂商调度、Web UI 监控 |

### 📥 下载

{download_table}

> 下载地址：{html_url}

### 🔑 校验

```
SHA256: {checksum}
```

### 💻 快速开始

```powershell
# === 1. 添加一个代理 ===
agent-proxy db add --url https://token.sensenova.cn/v1 `
                  --key sk-xxx --name sensenova

# === 2. 快速模式启动 ===
agent-proxy run --mode simple --host 0.0.0.0 --port 8080 --db 1

# === 3. 4 种协议任意调用 ===
# OpenAI Compatible
curl -X POST http://localhost:8080/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d '{{"model":"sensenova-6.7-flash-lite","messages":[{{"role":"user","content":"hello"}}]}}'

# Anthropic Messages
curl -X POST http://localhost:8080/v1/messages `
  -H "Content-Type: application/json" `
  -d '{{"model":"sensenova-6.7-flash-lite","max_tokens":1024,"messages":[{{"role":"user","content":"hello"}}]}}'

# Google Gemini
curl -X POST "http://localhost:8080/v1/models/sensenova-6.7-flash-lite:generateContent" `
  -H "Content-Type: application/json" `
  -d '{{"contents":[{{"role":"user","parts":[{{"text":"hello"}}]}}]}}'

# OpenAI Responses
curl -X POST http://localhost:8080/v1/responses `
  -H "Content-Type: application/json" `
  -d '{{"model":"sensenova-6.7-flash-lite","input":[{{"type":"message","role":"user","content":"hello"}}]}}'
```

### 📚 文档

- [README.md](https://github.com/{REPO}/blob/master/README.md)
- [MANUAL.md](https://github.com/{REPO}/blob/master/MANUAL.md) — 完整用户手册（协议兼容性、CLI 参考、Web UI、故障排查）
""",
        "draft": False,
        "prerelease": False,
    }

    release = gh_api("POST",
        f"https://api.github.com/repos/{REPO}/releases",
        release_body)

    upload_url = release["upload_url"].split("?")[0]
    print(f"Release created: {release['html_url']}")

    # Step 2: Upload binary
    result = upload_asset(upload_url, BINARY)
    print(f"Asset uploaded: {result.get('browser_download_url')}")
    print(f"\nDone! Open: {release['html_url']}")

if __name__ == "__main__":
    main()
