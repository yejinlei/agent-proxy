#!/usr/bin/env python3
"""Delete old release assets and upload new binary."""
import json, os, sys, urllib.request, urllib.error

REPO = "yejinlei/agent-proxy"
TAG = "v0.1.0"
BINARY = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "agent-proxy.exe")
TOKEN = os.environ.get("GH_TOKEN", "")

def api(method, url, data=None):
    body = json.dumps(data).encode() if data else None
    headers = {"Authorization": f"token {TOKEN}", "Accept": "application/vnd.github+json"}
    if body:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        print(f"  HTTP {e.code}: {e.read().decode()}", file=sys.stderr)
        return None

def upload_asset(url, path, token):
    content = open(path, "rb").read()
    fname = os.path.basename(path)
    full_url = url + "?name=" + fname
    boundary = "----WebKitFormBoundary7MA4YWxkTrZu0gW"
    body = (f"--{boundary}\r\n"
            + 'Content-Disposition: form-data; name="file"; filename="' + fname + '"\r\n'
            + "Content-Type: application/octet-stream\r\n\r\n").encode()
    body += content
    body += b"\r\n" + f"--{boundary}--\r\n".encode()
    headers = {"Authorization": f"token {token}",
               "Content-Type": f"multipart/form-data; boundary={boundary}",
               "Content-Length": str(len(body))}
    print(f"  Uploading {fname} ({len(content):,} bytes)...", flush=True)
    req = urllib.request.Request(full_url, data=body, headers=headers, method="POST")
    with urllib.request.urlopen(req, timeout=300) as resp:
        return json.loads(resp.read().decode())

def main():
    if not TOKEN:
        print("Set GH_TOKEN")
        sys.exit(1)

    # Get release
    release = api("GET", f"https://api.github.com/repos/{REPO}/releases/tags/{TAG}")
    if not release:
        sys.exit(1)
    print(f"Release: {release['html_url']}")

    # Delete existing assets
    for asset in release.get("assets", []):
        name = asset["name"]
        url = asset["url"]
        print(f"  Deleting old asset: {name}")
        api("DELETE", url)

    # Upload new binary
    result = upload_asset(release["upload_url"].split("{")[0], BINARY, TOKEN)
    if result:
        print(f"\nDone! {result['browser_download_url']}")
    else:
        sys.exit(1)

if __name__ == "__main__":
    main()
