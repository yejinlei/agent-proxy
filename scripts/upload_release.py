#!/usr/bin/env python3
"""Upload binary to existing GitHub Release."""
import json, os, sys, urllib.request, urllib.error

REPO = "yejinlei/agent-proxy"
TAG = "v0.1.0"
BINARY = os.path.join(os.path.dirname(os.path.abspath(__file__) or "."), "..", "agent-proxy.exe")
TOKEN = os.environ.get("GH_TOKEN", "")

def upload_asset(url, path, token):
    content = open(path, "rb").read()
    fname = os.path.basename(path)
    # GitHub requires filename in URL query param
    full_url = url + "?name=" + fname
    boundary = "----WebKitFormBoundary7MA4YWxkTrZu0gW"
    body = (f"--{boundary}\r\n"
            + 'Content-Disposition: form-data; name="file"; filename="' + fname + '"\r\n'
            + "Content-Type: application/octet-stream\r\n\r\n").encode()
    body += content
    body += b"\r\n" + f"--{boundary}--\r\n".encode()
    headers = {
        "Authorization": f"token {token}",
        "Content-Type": f"multipart/form-data; boundary={boundary}",
        "Content-Length": str(len(body)),
    }
    print(f"Uploading {fname} ({len(content):,} bytes / {len(content)//1024//1024} MB)...", flush=True)
    req = urllib.request.Request(full_url, data=body, headers=headers, method="POST")
    with urllib.request.urlopen(req, timeout=300) as resp:
        result = json.loads(resp.read().decode())
        print(f"OK: {result.get('browser_download_url')}", flush=True)
        return result

def main():
    if not TOKEN:
        print("Set GH_TOKEN env var")
        sys.exit(1)
    # Get release info
    url = f"https://api.github.com/repos/{REPO}/releases/tags/{TAG}"
    req = urllib.request.Request(url, headers={"Authorization": f"token {TOKEN}", "Accept": "application/vnd.github+json"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        release = json.loads(resp.read().decode())
    upload_url = release["upload_url"].split("{")[0]
    print(f"Release: {release['html_url']}")
    print(f"Upload URL: {upload_url}")
    result = upload_asset(upload_url, BINARY, TOKEN)
    print(f"\nDone!")

if __name__ == "__main__":
    main()
