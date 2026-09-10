#!/usr/bin/env bash
#
# The secret gate, proven in both directions.
#
# A scanner that reports nothing is indistinguishable from a scanner that
# detects nothing, and on 2026-09-08 this repository had the second one. The
# gate was green while this passed straight through it:
#
#     var leaked = "bz_test_sk_9Kq2mZx7Lp4Rt8Wn3Vc6Yb1Hd5Fg0Js"
#
# A working secret key, in a tracked file. The default gitleaks rules do not
# know the bz_*_sk_ format, and the generic heuristic only fires when a word
# like "key" or "secret" sits near the value — so assigning it to a variable
# named anything else was enough. The one credential class this system issues
# was the one class the gate did not look for.
#
# So the config is no longer trusted on the strength of its comments. Every
# credential class gets planted here as a realistic value and the gate must
# report it; every documented exception gets planted too and the gate must stay
# quiet. Both directions matter: an exception that hides a real credential is
# the same defect as a missing rule, and this suite exists because one of the
# exceptions did exactly that — a UUID allowlist written against the matched
# LINE excused everything else on that line, including a planted key.
#
# Run: tests/security/gitleaks-mutations.test.sh
set -euo pipefail

cd "$(dirname "$0")/../.."
CONFIG="$PWD/.gitleaks.toml"

if ! command -v gitleaks >/dev/null 2>&1; then
  echo "gitleaks not installed — skipping (the CI gate installs it)"
  exit 0
fi

WORK="$(mktemp -d)"
REPORT="$WORK/report.json"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

# Used by the assertions that are not a plant-and-scan.
ok() { printf '  \033[0;32m✓\033[0m %s\n' "$1"; pass=$((pass + 1)); }
no() { printf '  \033[0;31m✗\033[0m %s\n' "$1"; fail=$((fail + 1)); }

# Scans $WORK/tree and echoes the rule ids that fired, or nothing.
detect() {
  rm -f "$REPORT"
  gitleaks detect --no-git --source "$WORK/tree" --no-banner --redact \
    --config "$CONFIG" --report-format json --report-path "$REPORT" \
    >/dev/null 2>&1 || true
  [ -s "$REPORT" ] || { echo ""; return; }
  node -e '
    const f = require(process.argv[1]);
    process.stdout.write([...new Set(f.map(x => x.RuleID))].join("+"));
  ' "$REPORT"
}

plant() { # plant <relative-path> <line>
  rm -rf "$WORK/tree"
  mkdir -p "$WORK/tree/$(dirname "$1")"
  printf '%s\n' "$2" > "$WORK/tree/$1"
}

# A credential of this class must be reported, wherever it is written.
must_detect() { # must_detect <description> <path> <line>
  plant "$2" "$3"
  local got; got="$(detect)"
  if [ -n "$got" ]; then
    printf '  \033[0;32m✓\033[0m %-52s %s\n' "$1" "$got"
    pass=$((pass + 1))
  else
    printf '  \033[0;31m✗\033[0m %-52s NOT DETECTED\n' "$1"
    fail=$((fail + 1))
  fi
}

# A documented exception must not be reported.
must_allow() { # must_allow <description> <path> <line>
  plant "$2" "$3"
  local got; got="$(detect)"
  if [ -z "$got" ]; then
    printf '  \033[0;32m✓\033[0m %-52s allowed\n' "$1"
    pass=$((pass + 1))
  else
    printf '  \033[0;31m✗\033[0m %-52s reported as %s\n' "$1" "$got"
    fail=$((fail + 1))
  fi
}

# Synthetic throughout, and assembled at runtime rather than spelled out.
#
# Several of these prefixes are shared with real providers — whsec_ is Stripe's
# webhook secret, sbp_ is a Supabase personal access token, re_ is Resend — so a
# realistic value written as a literal here is indistinguishable, to a scanner,
# from a leaked one. GitHub push protection refused this file for the Supabase
# value and raised an alert for the Stripe-shaped one, which is both controls
# working exactly as intended.
#
# The answer is not to add them to an unblock list. A file whose whole purpose is
# to contain credential-shaped strings should not also be the file that trains
# everyone to wave alerts through. The bodies are built below, so nothing here
# reads as a credential at rest, and the rules under test only ever see the
# assembled value.
# bz_*_sk_ is this operator's own prefix and collides with no provider format,
# so it is the one value that can safely be written out in full.
KEY_BODY='9Kq2mZx7Lp4Rt8Wn3Vc6Yb1Hd5Fg0Js'
WHSEC_TOKEN="whsec_$(printf 'deadbeef%.0s' 1 2 3 4)"
SBP_TOKEN="sbp_$(printf 'deadbeef%.0s' 1 2 3 4 5)"
RESEND_TOKEN="re_$(printf 'deadbeef%.0s' 1 2 3)"
SUPABASE_TOKEN="sb_secret_$(printf 'deadbeef%.0s' 1 2 3)"
LEGACY_KEY="bz_test_$(printf 'deadbeef%.0s' 1 2 3 4 5 6 7 8)"

echo
echo "▸ A real credential must be reported, in any file and under any name"
must_detect "Banzami secret key, plainly assigned"  src/x.go   "var leaked = \"bz_test_sk_${KEY_BODY}\""
must_detect "Banzami secret key, live prefix"       src/l.go   "k := \"bz_live_sk_${KEY_BODY}\""
must_detect "Banzami secret key, in a comment"      src/c.go   "// TODO: remove bz_test_sk_${KEY_BODY}"
# The pre-sk_/pk_ format. One of these sat in a committed document — inside the
# block that gets pasted into App Store Connect — while every gate in this repo
# was green, because the rule for issued keys keys on an sk_ infix this format
# does not have.
must_detect "Banzami API key, legacy hex format"    doc/notes.md "API Key: ${LEGACY_KEY}"
must_detect "generated webhook signing secret"      src/y.py   "S = \"${WHSEC_TOKEN}\""
must_detect "Resend API key"                        src/z.sh   "RESEND=\"${RESEND_TOKEN}\""
must_detect "Supabase secret key"                   src/s.env  "K=${SUPABASE_TOKEN}"
must_detect "Supabase access token"                 src/t.env  "SBP=${SBP_TOKEN}"
must_detect "Postgres URL carrying a real password" src/p.env  "DATABASE_URL=postgres://banzami:Tr0ub4dor-Xk9-Qz@db.prod:5432/app"
must_detect "PEM private key block"                 src/k.pem  "-----BEGIN RSA PRIVATE KEY-----"

echo
echo "▸ An exception must excuse its own value and nothing else"
# This is the regression. The UUID exception was first written against the
# matched line, so a key appended to an excused line vanished with it.
must_detect "a key hidden after an excused retired id" \
  evidence/a.json \
  "\"key\": \"8a68d114-4556-41a5-8f56-f4c12b7b2e27 — REVOKED bz_test_sk_${KEY_BODY}\""
must_detect "a key hidden on a local dev database line" \
  infra/d.env \
  "DATABASE_URL=postgres://banzami:banzami_dev@localhost:5433/db  # bz_test_sk_${KEY_BODY}"

echo
echo "▸ Documented exceptions must stay quiet"
must_allow "RFC 6238 appendix-B published test vector"  src/t.go   "secret := \"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ\""
must_allow "local development database URL"             infra/d.env "DATABASE_URL=postgres://banzami:banzami_dev@localhost:5433/banzami_dev"
must_allow "local sandbox database URL"                 infra/s.env "DATABASE_URL=postgres://banzami:banzami_sandbox@postgres-sandbox:5432/banzami_sandbox"
must_allow "self-describing fixture webhook secret"     src/m.rs    "let s = \"whsec_webhook_signing_secret\";"
must_allow "forged key asserted to return 401"          src/e.mjs   "rec('unknown-key-fails', await me('bz_test_sk_deadbeefdeadbeefdeadbeefdeadbeef') === 401)"
must_allow "a retired resource id in release evidence"  evidence/b.json "\"key\": \"8a68d114-4556-41a5-8f56-f4c12b7b2e27 — REVOKED, confirmed rejected with 401\","
# Not an exception — the rule simply must not fire inside an identifier. These
# three real function names failed the security gate: "TestEnsure_Retries…"
# contains `re_` followed by twenty-odd alphanumerics. A gate that fails on
# ordinary code is a gate people learn to skip.
must_allow "a Go test name containing re_"             src/a_test.go "func TestEnsure_RetriesOnPublicReferenceCollision(t *testing.T) {"
must_allow "another identifier containing re_"         src/b_test.go "func TestEnsure_ConcurrentCallersConvergeOnOneProof(t *testing.T) {"
must_allow "a snake_case field containing re_"         src/c.go      "type X struct{ SignatureRe_ValueForAuditTrail string }"

echo
echo "▸ A directory exemption may only cover untracked content"
#
# A path exemption is absolute in a way a value exemption is not: a real
# credential inside node_modules/ or vendor/ is not reported, full stop. The
# whole justification is "those directories are gitignored, so nothing in them
# is repository exposure" — which is an assumption, and the kind that stops
# being true quietly, one `git add -f` at a time.
#
# So it is checked rather than assumed. For every directory this config excuses,
# git must hold nothing under it. File exemptions (lockfiles, the Firebase
# configs) are deliberately tracked and are justified by their content, not by
# being absent, so they are not part of this.
exempt_dirs="$(grep -oE "^  '''\(\^\|/\)[A-Za-z_.-]+/'''," "$CONFIG" \
  | sed -E "s@^  '''\(\^\|/\)@@; s@/''',\$@@")"
tracked_in_exempt=""
for d in $exempt_dirs; do
  n="$(git -C "$PWD" ls-files -- "*/$d/*" "$d/*" 2>/dev/null | wc -l | tr -d ' ')"
  [ "$n" != "0" ] && tracked_in_exempt="$tracked_in_exempt $d($n)"
done
if [ -z "$tracked_in_exempt" ]; then
  ok "every exempted directory holds no tracked files ($(echo $exempt_dirs | wc -w | tr -d ' ') checked)"
else
  no "exempted directories contain TRACKED files:$tracked_in_exempt"
fi

echo
if [ "$fail" -eq 0 ]; then
  printf '\033[0;32m✓ secret gate: %d/%d\033[0m\n\n' "$pass" "$((pass + fail))"
  exit 0
fi
printf '\033[0;31m✗ secret gate: %d of %d failed\033[0m\n' "$fail" "$((pass + fail))"
echo
echo "A 'NOT DETECTED' above means the gate would pass a real credential of that"
echo "class. A 'reported as' means a documented exception stopped working. Fix"
echo ".gitleaks.toml — do not adjust this suite to match it."
echo
exit 1
