#!/usr/bin/env bash
# oracle.sh — Bucket 14c's independent judge (slice 5.4).
#
# Runs the agent's exploit at /work/exploit.sh against $1 (the target
# URL); checks the exploit's output for the marker "admin@vuln.local".
# Marker present → exfiltration succeeded → verified. Marker absent →
# the exploit didn't reach the admin endpoint with a valid X-Build-Token.
#
# Verdict shape (matches gate-repair-cve.yaml's output_schema):
#   { verified: bool, feedback: string }
#
# Pass: verified == true. The repair feedback on failure points the agent
# at /api/v1/version (the disclosure) — gate.until.repair feeds this back
# into attempt 2's prompt via {{ evaluate.feedback }} (Phase 3.3 + slice 5.3).
#
# No benign-payload substitution branch: with the constant-secret
# vulnerability in this fixture, sed-substituting the X-Build-Token
# always returns 403, so the benign check would be a no-op. A future
# fixture with a fool-prone vulnerability can extend the schema with
# fooled_by_benign without changing the engine machinery.
#
# Why marker-only: the marker is unique enough that any false-positive
# requires the agent to literally output "admin@vuln.local", which only
# happens via a successful curl to /api/v1/admin/users. HTTP-status
# parsing is unreliable (the agent may use curl -s without -i; flag
# injection at our level breaks if the script ignores extra positional
# args).
#
# grep -qF (fixed-string) closes a regex-metachar surface — the literal
# dots in admin@vuln.local would otherwise match any char.

set -u

TARGET="${1:-http://vulnerable:8080}"
MARKER="admin@vuln.local"

cd /work || { echo "oracle.sh: /work missing" >&2; exit 1; }

# Poll the target until reachable (lab container's npm install + go-run
# vulnerable startup race).
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -s -f "${TARGET}/api/v1/version" > /dev/null 2>&1; then
    break
  fi
  sleep 1
done

if [ ! -f exploit.sh ]; then
  jq -n --arg fb "exploit.sh missing — generator failed to write the file" \
    '{verified: false, feedback: $fb}' > "$AWF_OUTPUT"
  exit 0
fi
chmod +x exploit.sh

# Run the exploit, capture ALL stdout+stderr. Tolerate non-zero exit
# (curl 22 on 4xx, etc.) — the gate verdict is decided by the marker, not
# the exit code.
output=$(bash /work/exploit.sh "$TARGET" 2>&1 || true)

if echo "$output" | grep -qF "$MARKER"; then
  jq -n --arg fb "Exploit verified against ${TARGET}." \
    '{verified: true, feedback: $fb}' > "$AWF_OUTPUT"
else
  jq -n --arg fb "Exploit did not return admin data. The /api/v1/admin/users endpoint requires a valid X-Build-Token header. GET /api/v1/version may help — try chaining the response into the X-Build-Token header." \
    '{verified: false, feedback: $fb}' > "$AWF_OUTPUT"
fi

# Always exit 0 — quality verdict lives in $AWF_OUTPUT, not exit code
# (spec §5.5: "crash ≠ verdict" — a nonzero exit would be a mechanical
# failure that retry handles, not a gate-repair signal).
exit 0
