#!/usr/bin/env python3
"""Update GitHub Release description by release ID."""
import json, os, sys, urllib.request, urllib.error

REPO = "yejinlei/agent-proxy"
TOKEN = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("GH_TOKEN", "")
BODY_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "release_body.json")

with open(BODY_FILE) as f:
    data = json.load(f)

# First get release by tag to find its ID
url = f"https://api.github.com/repos/{REPO}/releases/latest"
headers = {
    "Authorization": f"token {TOKEN}",
    "Content-Type": "application/json",
    "Accept": "application/vnd.github+json",
}
req = urllib.request.Request(url, headers=headers, method="GET")
with urllib.request.urlopen(req, timeout=30) as resp:
    release = json.loads(resp.read().decode())
    rid = release["id"]
    print(f"Release ID: {rid}, tag: {release['tag_name']}")

# Now PATCH with the correct URL format (numeric ID)
patch_url = f"https://api.github.com/repos/{REPO}/releases/{rid}"
body = json.dumps({"body": data["body"]}).encode()
req = urllib.request.Request(patch_url, data=body, headers=headers, method="PATCH")
try:
    with urllib.request.urlopen(req, timeout=30) as resp:
        result = json.loads(resp.read().decode())
        print(f"Updated: {result['html_url']}")
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}: {e.read().decode()}", file=sys.stderr)
    sys.exit(1)
