#!/usr/bin/env bash
# Finish the cleanup a run could not finish itself.
#
# A harness retires what it created from a trap, so a failed assertion or a
# Ctrl-C still cleans up. What a trap cannot survive is the machine going away
# underneath it — a killed container, a lost SSH session, an OOM. In that case
# the manifest is still on disk, listing by exact id everything that run owns,
# and this finishes the job from it.
#
# Manifests hold ids and nothing else. No secrets have ever been written to one
# and none may be: an id identifies a thing, a secret is authority over it.
#
#   bash tools/ops/cleanup-e2e-run.sh                 # list runs with residue
#   bash tools/ops/cleanup-e2e-run.sh <run-id>        # show what it owns
#   bash tools/ops/cleanup-e2e-run.sh <run-id> --apply
#   bash tools/ops/cleanup-e2e-run.sh --all --apply   # every stranded run
#
# Dry run by default, and it only ever touches ids that a manifest names. It
# cannot be pointed at "everything that looks like a fixture", which is the
# property that makes it safe to run without thinking hard.
set -uo pipefail
REMOTE="${BANZAMI_REMOTE:-root@217.160.9.248}"
E2E_STATE_DIR="${E2E_STATE_DIR:-/var/tmp/banzami-e2e}"

# The canonical remote-execution contract: prove the host, run there, and return
# the proof's own exit status. See tools/ops/lib/remote.sh.
for _p in "$(dirname "$0")/remote.sh" "$(dirname "$0")/lib/remote.sh" \
          "$(dirname "$0")/../lib/remote.sh" "$(dirname "$0")/../../tools/ops/lib/remote.sh"; do
  [ -f "$_p" ] && { . "$_p"; break; }
done
command -v remote_self_or_continue >/dev/null 2>&1 \
  || { echo "✗ tools/ops/lib/remote.sh not found — refusing to run without the host guard" >&2; exit 2; }
remote_self_or_continue "$@"

RUN=""; APPLY=0; ALL=0
for a in "$@"; do
  case "$a" in
    --apply) APPLY=1 ;;
    --all)   ALL=1 ;;
    *)       RUN="$a" ;;
  esac
done

# The library does the retiring; this only decides which manifests to hand it.
LIB="$(cd "$(dirname "$0")/../.." 2>/dev/null && pwd)/tests/phase0/lib/e2e-run.sh"
[ -f "$LIB" ] || LIB="/tmp/e2e-run.sh"
if [ ! -f "$LIB" ]; then
  echo "✗ the cleanup library is not on this machine (tests/phase0/lib/e2e-run.sh)" >&2
  echo "  copy the repository over, or run this from a checkout." >&2
  exit 2
fi
# shellcheck disable=SC1090
. "$LIB"

list_runs() { ls -1 "$E2E_STATE_DIR"/*.tsv 2>/dev/null | sed 's#.*/##; s#\.tsv$##'; }

if [ -z "$RUN" ] && [ "$ALL" -eq 0 ]; then
  echo "runs with residue in $E2E_STATE_DIR"
  n=0
  for r in $(list_runs); do
    printf '  %-18s %s resource(s)\n' "$r" "$(wc -l < "$E2E_STATE_DIR/$r.tsv" | tr -d ' ')"
    n=$((n+1))
  done
  [ "$n" -eq 0 ] && echo "  none — every run cleaned up after itself"
  exit 0
fi

RUNS="$RUN"; [ "$ALL" -eq 1 ] && RUNS=$(list_runs)
[ -n "$RUNS" ] || { echo "nothing to do"; exit 0; }

for r in $RUNS; do
  M="$E2E_STATE_DIR/$r.tsv"
  [ -f "$M" ] || { echo "✗ no manifest for run $r"; continue; }
  echo "run $r owns:"
  awk -F'\t' '{printf "  %-18s %s\n", $1, $2}' "$M"
  if [ "$APPLY" -eq 0 ]; then
    echo "  (dry run — re-run with --apply)"
    continue
  fi
  # Reuse the harness cleanup path exactly, so recovery and normal cleanup can
  # never drift apart into two behaviours.
  E2E_RUN_ID="$r"
  E2E_MANIFEST="$M"
  # The same discovery the run itself does, not a subset of it. This used to be
  # a hand-copied four lines missing the database context that retirement reads,
  # so recovery either died on an unbound variable or silently decided every
  # link was already terminal.
  e2e_discover
  e2e_end
done
