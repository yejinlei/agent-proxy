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
    """Upload release asset."""
    content = open(path, "rb").read()
    headers = {
        "Authorization": f"token {TOKEN}",
        "Content-Type": "application/octet-stream",
        "Content-Length": str(len(content)),
    }
    req = urllib.request.Request(url, data=content, headers=headers, method="POST")
    fname = os.path.basename(path)
    print(f"  Uploading {fname} ({len(content):,} bytes / {len(content)//1024//1024} MB)...")
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
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
        "body": f"""## agent-proxy {TAG}

### 下载

{download_table}

> 下载地址：{html_url}

### 校验

```
SHA256: {checksum}
```

### 快速开始

```powershell
# 复杂模式
$env:AGENT_PROXY_API_KEY = "sk-your-key"
.\\agent-proxy.exe

# 快速模式
.\\agent-proxy.exe add --url https://token.sensenova.cn/v1 --key sk-xxx --name sensenova
.\\agent-proxy.exe --db 1
```

### 文档

- [README.md](https://github.com/{REPO}/blob/master/README.md)
- [MANUAL.md](https://github.com/{REPO}/blob/master/MANUAL.md)
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
