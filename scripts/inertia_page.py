"""Print the Inertia page object (component + props) embedded in a Laravel/Inertia HTML response.

Inertia v3 puts it in <script data-page="app" type="application/json">; older versions in a
data-page="..." attribute on the root element.
"""
import html
import json
import re
import sys

body = sys.stdin.read()
m = re.search(r'<script[^>]*data-page="[^"]*"[^>]*>(.*?)</script>', body, re.S)
if m:
    print(json.dumps(json.loads(m.group(1)), ensure_ascii=False))
    sys.exit(0)
m = re.search(r'data-page="(\{[^"]*)"', body)
print(json.dumps(json.loads(html.unescape(m.group(1))), ensure_ascii=False) if m else "{}")
