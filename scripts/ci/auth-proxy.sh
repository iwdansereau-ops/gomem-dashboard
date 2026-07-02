#!/usr/bin/env bash
# auth-proxy.sh — tiny local HTTP proxy that forwards every request to
# $UPSTREAM with an Authorization: Bearer $TOKEN header injected. Used by
# the staging-memory-check workflow so we don't have to teach the gomem
# capture CLI about auth.
#
# Usage: auth-proxy.sh <upstream-base-url> <bearer-token> <listen-port>
set -euo pipefail

UPSTREAM="${1:?upstream URL required}"
TOKEN="${2:?bearer token required}"
PORT="${3:-17070}"

# Prefer socat for a persistent, per-request proxy; fall back to a tiny
# python one-liner if socat isn't installed on the runner.
if command -v socat >/dev/null 2>&1 && command -v curl >/dev/null 2>&1; then
  # Handler script socat will spawn for every accepted connection.
  HANDLER="$(mktemp)"
  cat >"$HANDLER" <<HANDLER_EOF
#!/usr/bin/env bash
set -euo pipefail
# Read the request line + headers into an array; strip \r.
mapfile -t lines
req_line="\${lines[0]//$'\r'/}"
method="\$(awk '{print \$1}' <<<"\$req_line")"
path="\$(awk '{print \$2}'  <<<"\$req_line")"
resp="\$(curl -sS --max-time 60 -X "\$method" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Accept: */*" \
  --output - --write-out '\n---STATUS---%{http_code}' \
  "$UPSTREAM\$path" || true)"
body="\${resp%%\$'\n'---STATUS---*}"
code="\${resp##*---STATUS---}"
printf 'HTTP/1.1 %s OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n' \
  "\$code" "\${#body}"
printf '%s' "\$body"
HANDLER_EOF
  chmod +x "$HANDLER"
  exec socat "TCP-LISTEN:${PORT},reuseaddr,fork" "EXEC:${HANDLER}"
else
  # Fallback: python http.server-based proxy.
  exec python3 - "$UPSTREAM" "$TOKEN" "$PORT" <<'PY'
import sys, urllib.request, urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
UP, TOK, PORT = sys.argv[1], sys.argv[2], int(sys.argv[3])
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        url = UP.rstrip("/") + self.path
        req = urllib.request.Request(url, headers={"Authorization": f"Bearer {TOK}"})
        try:
            with urllib.request.urlopen(req, timeout=90) as r:
                data = r.read()
                self.send_response(r.status)
                for k, v in r.headers.items():
                    if k.lower() in ("transfer-encoding", "content-encoding"):
                        continue
                    self.send_header(k, v)
                self.end_headers()
                self.wfile.write(data)
        except urllib.error.HTTPError as e:
            self.send_response(e.code)
            self.end_headers()
            self.wfile.write(e.read())
    def log_message(self, *a, **k):  # quiet
        pass
ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
PY
fi
