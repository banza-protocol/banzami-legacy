#!/usr/bin/env bash
# Banzami Environment Blueprint — provenance-first Sandbox deployment orchestrator (adapter D).
#
# Deploys EXACTLY the four approved services from a verified release package into the current
# generated Sandbox project: no build, no pull, no mutable tags. Each service is validated
# (allowlist + manifest revision + loaded-image digest match + SBOM/provenance + no secret)
# BEFORE deployment; health is validated AFTER. The DB credential is delivered file-only and
# exported in-process by a narrow wrapper so it never appears in Docker-inspectable config.
# Internal networking only; non-root; no host namespaces/ports/socket/mounts.
#
# Subcommands: plan | apply | verify | clean
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
SVC_EVIDENCE="$REPO_ROOT/infra/blueprint/service-lab/scripts/validate-service-evidence.mjs"
SANDBOX_STATE="${TMPDIR:-/tmp}/banzami-blueprint-sandbox/current.run"
RELEASE_STATE="${TMPDIR:-/tmp}/banzami-blueprint-release/current.run"
LABEL="com.banzami.blueprint.sandbox-deploy"
# APPROVED four services: name|port|binary
SERVICES=(
  "core-api-staging|8081|core-api"
  "api-gateway-staging|8080|api-gateway"
  "developer-api|8086|developer-api"
  "public-api-staging|8083|public-api"
  # pay-frontend — the hosted payer surface (Banzami ADR-052, CAP-APP-004).
  #
  # It was previously forbidden here. That invariant was written when the
  # Sandbox project was API-only; the external Sandbox product now includes the
  # page a payer actually opens, and every payment link the platform issues
  # points at it. A surface the product requires does not belong in a fourth
  # standalone topology to preserve a rule that no longer describes the product.
  #
  # It is the ONLY entry here with no financial authority: no secret mount, no
  # database URL, no Core credential. See PAY_FRONTEND_APP_PLANE_ONLY below.
  # The third field is what the entrypoint execs. For the Go services it is a
  # binary; for this one it is a Next.js standalone server, and `node` alone is
  # a REPL. Deployed that way the container started, found stdin was not a
  # terminal, exited 0 in under a second, and pay.banzami.com answered 502 to
  # every payer for twelve hours. A clean exit code is what made it quiet:
  # nothing crashed, nothing restarted, no log line was written.
  "pay-frontend|3002|node server.js"
  # The operator console (Stage D, approved 2026-09-08). Two entries, because it
  # is two things: an API that reads this Sandbox database, and a browser app.
  #
  # It is here rather than in a separate topology for the same reason
  # pay-frontend is: the operator console is part of the released Sandbox
  # product — a platform whose routine operations require psql is not a platform
  # — and it reads exactly the database these services already write.
  #
  # ENVIRONMENT=SANDBOX is not decoration. admin-api used to label its primary
  # database LIVE, and the primary database here is banzami_staging; the console
  # would have reported Sandbox balances and compliance cases as real money.
  "admin-api|8082|admin-api"
  "admin-frontend|3002|node server.js"
)
# Services that must never exist in this project.
#
# `pay` and `frontend` were removed as blanket terms: they forbade the hosted
# payer surface, which is now an authorised Sandbox application (above). What
# they were really protecting — that no ADMIN or LIVE surface is deployed here —
# is unchanged and named precisely instead of by substring.
# admin-api and admin-frontend left this list when Stage D approved the operator
# console for the Sandbox. What the list protects is unchanged: no LIVE surface
# and no merchant dashboard in this project. admin-api-staging stays forbidden
# because it does not exist — a second admin under a different name would be a
# second source of operator truth.
FORBIDDEN="admin-api-staging dashboard-frontend checkout-frontend reverse-proxy banza-docs banzai"

# pay-frontend gets the APPLICATION plane only.
#
# Every other service is on the data plane too, because every other service
# talks to Postgres. This one must not: it holds no credential and reads the
# gateway over HTTP like any other client would. Adding a frontend must not
# broaden what the Sandbox exposes.
PAY_FRONTEND_APP_PLANE_ONLY=1

die() { echo "sandbox-deploy: $*" >&2; exit 1; }
hold() { echo "$1"; exit "${2:-43}"; }
allow_ok() { local n="$1" e; for e in "${SERVICES[@]}"; do [ "${e%%|*}" = "$n" ] && return 0; done; return 1; }
mget() { grep -E "^$1=" "$MANIFEST" | head -1 | cut -d= -f2-; }
oci_digest() { OCI="$1" node -e 'const fs=require("fs"),p=require("path");const oci=process.env.OCI;const b=d=>JSON.parse(fs.readFileSync(p.join(oci,"blobs",d.split(":")[0],d.split(":")[1])));const t=JSON.parse(fs.readFileSync(p.join(oci,"index.json")));let img=null;const v=d=>{const m=d.mediaType||"";if(m.includes("image.index"))b(d.digest).manifests.forEach(v);else if(m.includes("image.manifest")&&(d.annotations||{})["vnd.docker.reference.type"]!=="attestation-manifest")img=img||d.digest;};t.manifests.forEach(v);process.stdout.write(img||"");'; }

load_context() {
  [ -f "$SANDBOX_STATE" ] || die "no bootstrapped Sandbox"
  [ -f "$RELEASE_STATE" ] || die "no verified release package"
  . "$SANDBOX_STATE"; . "$RELEASE_STATE"
  : "${BZSB_PROJECT:?}" "${BZSB_DATA_NET:?}" "${BZSB_APP_NET:?}" "${BZSB_SECRET_ROOT:?}" "${EVIDENCE_ROOT:?}"
  : "${RELEASE_ROOT:?}" "${SOURCE_REVISION:?}"
  MANIFEST="$RELEASE_ROOT/manifest.txt"; [ -f "$MANIFEST" ] || die "release manifest missing"
  DBURL_FILE="$EVIDENCE_ROOT/db_url"
  JWT_FILE="$EVIDENCE_ROOT/jwt_secret"
  CIK_FILE="$EVIDENCE_ROOT/core_internal_key"
  # Developer/platform API-key layer (ADR-046/047) synthetic credentials — file-only.
  APIKEY_PEPPER_FILE="$EVIDENCE_ROOT/api_key_pepper"
  DEVINT_FILE="$EVIDENCE_ROOT/developer_internal_key"
  PAYEEVAL_FILE="$EVIDENCE_ROOT/core_payee_validation_key"
  SESSION_FILE="$EVIDENCE_ROOT/session_secret"
  OTP_FILE="$EVIDENCE_ROOT/otp_pepper"
  # Operator console (Stage D). Signs BANZADMIN sessions; regenerating it signs
  # every operator out, so it is preserved across applies like the rest.
  ADMINJWT_FILE="$EVIDENCE_ROOT/admin_jwt_secret"
}
svc_image() { docker image ls --filter "label=com.banzami.blueprint.service-lab.service=$1" --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | head -1; }

# provenance-first validation for one service; echoes the loaded image tag on success
validate_service() { # <name>
  local name="$1" oci="$RELEASE_ROOT/images/$1.oci" tag
  allow_ok "$name" || die "service $name not allowlisted"
  echo "$FORBIDDEN" | tr ' ' '\n' | grep -qx "$name" && die "service $name is forbidden"
  [ -d "$oci" ] || die "$name image missing from package"
  [ "$(mget source_revision)" = "$SOURCE_REVISION" ] || die "manifest revision != canonical"
  # provenance validation BEFORE load/deploy
  node "$SVC_EVIDENCE" "$oci" "$SOURCE_REVISION" "$name" >/dev/null 2>&1 || { echo "  $name provenance FAIL" >&2; return 1; }
  # load from package (no build, no pull) + digest match manifest
  tar -cf "$RELEASE_ROOT/$name.tar" -C "$oci" .
  docker load -i "$RELEASE_ROOT/$name.tar" >/dev/null 2>&1 || die "$name load failed"
  [ "$(oci_digest "$oci")" = "$(mget "service.$name.image_digest")" ] || die "$name loaded digest != manifest"
  tag="$(svc_image "$name")"; [ -n "$tag" ] || die "$name image not resolvable after load"
  # no secret in image metadata
  docker image inspect "$tag" -f '{{json .Config.Env}}{{json .Config.Labels}}' | grep -qiE 'password=|secret=|database_url=[a-z]|private[_-]?key' && die "$name image carries a secret"
  # no collision (already-running same-name in project)
  docker ps -aq --filter "name=${BZSB_PROJECT}-$name" | grep -q . && die "$name already deployed (collision)"
  printf '%s' "$tag"
}

write_db_url() { # file-only runtime credential (bl_app_runtime → banzami_staging), never in env
  local proto='postgresql://' host='postgres:5432' db='banzami_staging'
  # 0644 (not 0600): bind-mounted read-only into NON-root service containers; on Linux the mount
  # preserves host perms so a 0600 root-owned file is unreadable by the container user. Host
  # confidentiality is preserved by the 0700 root-only EVIDENCE_ROOT dir that contains it.
  printf '%sbl_app_runtime:%s@%s/%s' "$proto" "$(cat "$BZSB_SECRET_ROOT/mi_runtime")" "$host" "$db" > "$DBURL_FILE"; chmod 0644 "$DBURL_FILE"
}
uuid() { uuidgen 2>/dev/null | tr 'A-Z' 'a-z' || python3 -c 'import uuid;print(uuid.uuid4())'; }

# keep_or_mint — write a synthetic credential ONLY if the file is not already there.
#
# Every one of these used to be regenerated on each apply, which reads as
# harmless for "disposable, per-run" secrets and is not. api_key_pepper is what
# every issued API key is hashed with: minting a new one silently invalidates
# every key that exists — including keys installed in other people's production
# environments, which cannot be re-read and have to be reissued by hand.
# session_secret does the same to every signed-in Console session, and
# core_internal_key to the service pairs mid-flight.
#
# So an apply now preserves what is already there. Deliberate rotation is a
# deliberate act: BZSB_ROTATE_SECRETS=1 (or deleting the file) mints a new one,
# and the caller is expected to know what has to be reissued afterwards.
keep_or_mint() { # <file> <label>
  local f="$1" label="$2"
  if [ -f "$f" ] && [ -s "$f" ] && [ "${BZSB_ROTATE_SECRETS:-0}" != "1" ]; then
    # Re-assert the mode even when keeping the value. See secret_mode below.
    chmod 0644 "$f"
    echo "  $label kept (already provisioned)"
    return 0
  fi
  printf '%s%s' "$(uuid)" "$(uuid)" | tr -d '-' > "$f"; chmod 0644 "$f"
  echo "  $label minted"
}

# assert_secret_modes — every mounted secret must be readable by the service user.
#
# All five services run non-root. A bind mount preserves the host's permissions,
# so a 0600 or 0700 root-owned file is simply unreadable inside the container —
# and the service does not crash. It starts, logs one warning, and runs without
# the credential: admin-api came up healthy with "ADMIN_JWT_SECRET not set —
# operator login disabled (503)" and no mailer, and the only visible symptom was
# an operator who could not sign in with a password that was correct.
#
# Worse, it is latent. A running process has already read its secrets, so a mode
# change breaks nothing until the next restart — which is how every service on
# the stack ended up one restart away from the same failure.
#
# Confidentiality lives in the 0700 root-only directory these files sit in, not
# in the file bits, exactly as write_db_url has always documented. This asserts
# that on every deploy rather than trusting whatever last touched them.
assert_secret_modes() { # <dir>
  local d="$1" f fixed=0
  [ -d "$d" ] || return 0
  for f in "$d"/*; do
    [ -f "$f" ] || continue
    case "$(stat -c '%a' "$f" 2>/dev/null || stat -f '%Lp' "$f" 2>/dev/null)" in
      644) ;;
      *) chmod 0644 "$f"; fixed=$((fixed+1)) ;;
    esac
  done
  [ "$fixed" -gt 0 ] && echo "  secret_modes_reasserted ($fixed file(s) were unreadable by the non-root service user)"
  return 0
}
# file-only synthetic signing secret for services that hard-require JWT_SECRET at
# boot (e.g. public-api). Disposable, generated per run, NOT a real credential;
# delivered file-only + exported in-process so it never lands in Docker config.
write_jwt_secret() { keep_or_mint "$JWT_FILE" jwt_secret; }
# file-only synthetic Gateway↔Core service credential (X-Internal-Key ↔ CORE_INTERNAL_KEY).
# Enables the fail-closed internal route groups (refunds, F4) inside the Sandbox. Disposable,
# generated per run, delivered file-only + exported in-process so it never lands in Docker
# config; the SAME value is mounted into every service so the shared secret matches.
write_core_internal_key() { keep_or_mint "$CIK_FILE" core_internal_key; }
# file-only synthetic credentials for the developer/platform API-key layer. All
# disposable, generated per run, delivered file-only + exported in-process so they
# never land in Docker-inspectable config. The shared ones (developer_internal_key,
# core_payee_validation_key) are the SAME value in every service so the pairs match.
# Sandbox/Phase-0 only; the fixture + dev-key paths are hard-gated to ENVIRONMENT=sandbox.
write_devkey_secrets() {
  # api_key_pepper first, and named, because it is the one whose regeneration is
  # invisible: nothing fails at deploy time, and every API key in the world stops
  # verifying the moment developer-api restarts.
  keep_or_mint "$APIKEY_PEPPER_FILE" api_key_pepper
  keep_or_mint "$DEVINT_FILE"        developer_internal_key
  keep_or_mint "$PAYEEVAL_FILE"      core_payee_validation_key
  keep_or_mint "$SESSION_FILE"       session_secret
  keep_or_mint "$OTP_FILE"           otp_pepper
}

# ensure_proof_signing_key — the operator key that makes a public proof an
# attestation rather than a row.
#
# A transaction proof carries an HMAC over its canonical payload. Unkeyed, that
# HMAC is computable by anyone who knows the format, so it proves nothing — and
# the gateway's own SEC-003 gate says so, refusing to start in LIVE without it
# while warning and continuing in Sandbox. The Sandbox warned and continued for
# its whole life, so every receipt on a public surface real people download was
# going to be signed with the empty key.
#
# Kept once provisioned, and deliberately NOT rotated by BZSB_ROTATE_SECRETS.
# Every other credential here can be rotated with a known cost — reissue keys,
# sign everyone out. This one cannot: a proof is an immutable public record, and
# a new key does not invalidate a session, it invalidates the operator's
# signature on every receipt ever issued. Rotating it is a decision that needs a
# re-signing plan, not a flag.
#
# It is mounted into every service that already holds credentials rather than
# into the gateway alone, matching how every shared secret here is delivered.
# Only the gateway reads it.
ensure_proof_signing_key() { # <secret_dir>
  local d="$1" f="$1/bzm_proof_signing_key"
  [ -d "$d" ] || return 0
  if [ ! -s "$f" ]; then
    printf '%s%s' "$(uuid)" "$(uuid)" | tr -d '-' > "$f"
    echo "  bzm_proof_signing_key minted"
  fi
  chmod 0644 "$f"
}

deploy_one() { # <name> <port> <binary> <tag>
  local name="$1" port="$2" bin="$3" tag="$4" cname="${BZSB_PROJECT}-$name"
  # The gateway resolves the Developer API by its canonical in-cluster host
  # `developer-api` (SSRF-guarded allowlist); give that container the alias.
  local alias_args=(); [ "$name" = "developer-api" ] && alias_args=(--network-alias developer-api)
  # synthetic NON-secret config; secrets are file-only + exported in-process (never -e)
  # `docker create` + attach + `start`, never `docker run -d` followed by a
  # `network connect`. Attaching the second network to an ALREADY-RUNNING
  # container reconfigures Docker's embedded resolver underneath it, and a
  # service that resolves a dependency during boot can catch the reconfiguration
  # window and get SERVFAIL. Observed reproducibly: public-api died at startup
  # with `lookup postgres on 127.0.0.11:53: server misbehaving`, twice, while the
  # gateway and developer-api survived only because they do not hard-fail on a
  # boot-time database ping. The container now has every network before its first
  # instruction runs.
  local cfg_args=(); local cfg_line
  while IFS= read -r cfg_line; do [ -n "$cfg_line" ] && cfg_args+=(-e "$cfg_line"); done < <(release_config_env "$name")
  docker create --name "$cname" --network "$BZSB_DATA_NET" "${alias_args[@]}" "${cfg_args[@]}" \
    --label "$LABEL=1" --label "$LABEL.run=$BZSB_PROJECT" --label "$LABEL.service=$name" \
    --security-opt "no-new-privileges:true" \
    -v "$DBURL_FILE:/run/secrets/db_url:ro" \
    -v "$JWT_FILE:/run/secrets/jwt_secret:ro" \
    -v "$CIK_FILE:/run/secrets/core_internal_key:ro" \
    -v "$APIKEY_PEPPER_FILE:/run/secrets/api_key_pepper:ro" \
    -v "$DEVINT_FILE:/run/secrets/developer_internal_key:ro" \
    -v "$PAYEEVAL_FILE:/run/secrets/core_payee_validation_key:ro" \
    -v "$SESSION_FILE:/run/secrets/session_secret:ro" \
    -v "$OTP_FILE:/run/secrets/otp_pepper:ro" \
    -e "CORE_API_PORT=$port" -e "PORT=$port" -e "ENVIRONMENT=sandbox" \
    -e "BANZAMI_PILOT_LIMITS=1" \
    -e "CORE_API_URL=http://${BZSB_PROJECT}-core-api-staging:8081" \
    -e "DEVELOPER_API_URL=http://developer-api:8086" \
    -e "DEVELOPER_KEY_AUTH_ENABLED=true" -e "PAYMENT_CAPABILITY_RELEASED=true" \
    -e "REDIS_URL=redis://redis:6379" -e "REDIS_ADDR=redis:6379" \
    -e "TRANSIT_ACCOUNT_ID=$(uuid)" -e "BANK_ACCOUNT_ID=$(uuid)" -e "OPERATOR_FEE_REVENUE_ACCOUNT_ID=$(uuid)" \
    --entrypoint sh "$tag" -c 'export DATABASE_URL="$(cat /run/secrets/db_url)"; export JWT_SECRET="$(cat /run/secrets/jwt_secret)"; export CORE_INTERNAL_KEY="$(cat /run/secrets/core_internal_key)"; export CORE_REFUND_KEY="$CORE_INTERNAL_KEY"; export API_KEY_PEPPER="$(cat /run/secrets/api_key_pepper)"; export DEVELOPER_INTERNAL_KEY="$(cat /run/secrets/developer_internal_key)"; export CORE_PAYEE_VALIDATION_KEY="$(cat /run/secrets/core_payee_validation_key)"; export SESSION_SECRET="$(cat /run/secrets/session_secret)"; export OTP_PEPPER="$(cat /run/secrets/otp_pepper)"; exec '"$bin" >/dev/null 2>&1 || return 1
  docker network connect "$BZSB_APP_NET" "$cname" >/dev/null 2>&1 || true
  docker start "$cname" >/dev/null 2>&1 || return 1
  # health AFTER deployment (docker HEALTHCHECK from the image)
  local i=0 st
  while :; do st="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}nohc{{end}}' "$cname" 2>/dev/null)"
    [ "$st" = healthy ] && return 0
    [ "$st" = nohc ] && { docker exec "$cname" true >/dev/null 2>&1 && return 0; }
    i=$((i+1)); [ "$i" -gt 60 ] && { echo "  $name health timeout (last=$st)" >&2; docker logs --tail 20 "$cname" 2>&1 | sed 's/^/    /' >&2; return 1; }
    sleep 2
  done
}

cmd_plan() {
  echo "sandbox-deploy PLAN: deploy 4 approved services one-at-a-time from the verified package"
  echo "  provenance-before-health; no build/pull/mutable-tag; file-only in-process DB credential; non-root; internal networks only"
  echo "  services: core-api-staging api-gateway-staging developer-api public-api-staging (allowlist-enforced)"
}

cmd_apply() {
  load_context; write_db_url; write_jwt_secret; write_core_internal_key; write_devkey_secrets
  local e name port bin tag
  for e in "${SERVICES[@]}"; do
    IFS='|' read -r name port bin <<<"$e"
    echo "sandbox-deploy: validating + deploying $name"
    tag="$(validate_service "$name")" || hold "BLOCKER — SANDBOX DEPLOYMENT CONTRACT CANNOT BE VALIDATED" 43
    echo "  $name provenance_validated PASS"
    if deploy_one "$name" "$port" "$bin" "$tag"; then echo "  $name deployed_and_healthy PASS"; else echo "  $name deployed_and_healthy FAIL"; hold "BLOCKER — SANDBOX DEPLOYMENT CONTRACT CANNOT BE VALIDATED" 43; fi
  done
  echo "sandbox-deploy: all four approved services deployed and healthy"
}

cmd_verify() {
  load_context; local rc=0 e name port bin cname
  for e in "${SERVICES[@]}"; do
    IFS='|' read -r name port bin <<<"$e"; cname="${BZSB_PROJECT}-$name"
    docker ps -q --filter "name=$cname" | grep -q . || { echo "  $name running FAIL"; rc=1; continue; }
    # health
    local st; st="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}nohc{{end}}' "$cname")"
    { [ "$st" = healthy ] || { [ "$st" = nohc ] && docker exec "$cname" true >/dev/null 2>&1; }; } && echo "  $name healthy PASS" || { echo "  $name healthy FAIL"; rc=1; }
    # non-root
    [ "$(docker exec "$cname" id -u 2>/dev/null)" != "0" ] && echo "  $name non_root PASS" || { echo "  $name non_root FAIL"; rc=1; }
    # no secret in inspectable env / no host port
    docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$cname" | grep -qiE 'DATABASE_URL=|password=|:[^ ]*@' && { echo "  $name no_secret_in_env FAIL"; rc=1; } || echo "  $name no_secret_in_env PASS"
    [ -z "$(docker port "$cname" 2>/dev/null)" ] && echo "  $name no_host_port PASS" || { echo "  $name no_host_port FAIL"; rc=1; }
    # image identity matches manifest
    [ "$(oci_digest "$RELEASE_ROOT/images/$name.oci")" = "$(mget "service.$name.image_digest")" ] && echo "  $name image_identity_matches PASS" || { echo "  $name image_identity_matches FAIL"; rc=1; }
  done
  echo "SANDBOX_DEPLOY_VERIFY_RESULT: $([ "$rc" -eq 0 ] && echo PASS || echo FAIL)"; return "$rc"
}

cmd_clean() {
  load_context 2>/dev/null || true
  [ -n "${BZSB_PROJECT:-}" ] && docker ps -aq --filter "label=$LABEL.run=$BZSB_PROJECT" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker ps -aq --filter "label=$LABEL" | xargs -r docker rm -f >/dev/null 2>&1 || true
  local e name; for e in "${SERVICES[@]}"; do name="${e%%|*}"; docker image ls --filter "label=com.banzami.blueprint.service-lab.service=$name" -q | xargs -r docker image rm -f >/dev/null 2>&1 || true; done
  rm -f "${DBURL_FILE:-/nonexistent}" "${JWT_FILE:-/nonexistent}" "${CIK_FILE:-/nonexistent}" \
        "${APIKEY_PEPPER_FILE:-/nonexistent}" "${DEVINT_FILE:-/nonexistent}" "${PAYEEVAL_FILE:-/nonexistent}" \
        "${SESSION_FILE:-/nonexistent}" "${OTP_FILE:-/nonexistent}" 2>/dev/null || true
  echo "sandbox-deploy: scoped cleanup done"
}

# cmd_deploy_one — redeploy a SINGLE approved service from an already-built local image
# tag (the fast source-bundle native-build path). It CLONES the currently-running
# container's configuration (its file-only secret mounts, non-secret -e env, networks
# and non-root/no-new-privileges posture) and swaps only the image — so it is decoupled
# from the gated bootstrap state, does NOT regenerate secrets (cross-service auth is
# preserved), and does NOT run any migration, VM reset or prune. The file-only-secret
# entrypoint is reconstructed (secrets are exported in-process from /run/secrets, never
# via -e). Rollback redeploys the previously-running image.
# release_config_env — non-secret configuration the RELEASE is the authority on.
#
# Everything else about a redeployed container is cloned from its predecessor,
# which is right for secrets and networks and wrong for configuration: a new
# setting introduced by a commit would never reach the Sandbox, because the
# running container has never heard of it and the clone faithfully reproduces
# its absence. Deploying the code that reads PAY_BASE_URL therefore changed
# nothing at all, and the only way to pick it up would have been a full gated
# bootstrap — which regenerates secrets.
#
# So these are declared once, used by both the bootstrap create and the
# single-service swap, and re-applied on every deploy: the release wins over
# whatever the previous container happened to carry.
release_config_env() {
  case "$1" in
    admin-api)
      # The console must label what it shows for what it is. Without this the
      # primary database defaults to LIVE, and the primary database here is the
      # Sandbox one.
      echo "ENVIRONMENT=SANDBOX"
      # Mail configuration, re-applied on every deploy. deploy-one clones the
      # previous container's env, so a setting introduced later would otherwise
      # never arrive — and BANZADMIN without a mailer hands password-reset links
      # back through the API instead of sending them.
      echo "EMAIL_PROVIDER=resend"
      # Defaults to true, which is the right default and the wrong setting here:
      # a password-reset link that is only logged never reaches the operator.
      echo "EMAIL_DRY_RUN=false"
      echo "EMAIL_FROM_NAME=Banzami"
      echo "EMAIL_FROM_ADDRESS=contact@banzami.com"
      echo "EMAIL_NOREPLY_NAME=Banzami"
      echo "EMAIL_NOREPLY_ADDRESS=noreply@banzami.com"
      echo "EMAIL_REPLY_TO=contact@banzami.com"
      echo "ADMIN_BASE_URL=https://admin.banzami.com"
      ;;
    api-gateway-staging)
      # The origin of the hosted payer surface (ADR-052). Without it, every
      # payment link the API hands an integration points at the gateway's own
      # JSON route instead of a page a person can pay on.
      echo "PAY_BASE_URL=https://pay.banzami.com"
      ;;
    public-api-staging)
      # Where public-api asks the Gateway to mint a transaction proof.
      #
      # The Gateway owns proof generation (it holds the signing key and hash
      # salt), so public-api calls /internal/v1/proofs/ensure when a consumer
      # asks for a receipt. INTERNAL_API_KEY already arrives from
      # /run/secrets/core_internal_key; this URL never did, and NewProofClient
      # returns nil when either half is missing.
      #
      # The consequence was silent and total: every consumer receipt fell back
      # to a reference derived from the transfer id, no proof was ever minted,
      # and transaction_proofs stayed empty — so every receipt Banzami issued
      # advertised a verification page that answered "does not exist or may have
      # been forged". Receipt issuance now fails closed instead of falling back,
      # which turns that missing URL from a silent wrong answer into a visible
      # refusal; this is the setting that makes it succeed.
      # Resolved from the running Gateway rather than from BZSB_PROJECT: the
      # single-service swap path does not carry that variable, and a config value
      # that is correct only on a full bootstrap is the failure mode this whole
      # function exists to prevent.
      local gw
      # || true: pipefail turns an empty grep into a failed command substitution,
      # which under set -e kills the deploy — the same trap already documented at
      # the top of cmd_deploy_one.
      gw="$(docker ps --format '{{.Names}}' | grep -E -- '-api-gateway-staging$' | head -1 || true)"
      [ -n "$gw" ] || gw="${BZSB_PROJECT:-}-api-gateway-staging"
      echo "GATEWAY_INTERNAL_URL=http://${gw}:8080"
      ;;
  esac
}

cmd_deploy_one() {
  local name="$1" tag="$2" rollback="${3:-}"
  local e n p b bin port; for e in "${SERVICES[@]}"; do IFS='|' read -r n p b <<<"$e"; [ "$n" = "$name" ] && { bin="$b"; port="$p"; }; done
  [ -n "${bin:-}" ] || die "unknown sandbox service: $name"
  # `|| true`: under `set -e` an empty grep result exits the script, which is
  # exactly the case a FIRST deploy is — no container yet. Without it the script
  # died silently before reaching the create path below, reporting only rc=1.
  local cname; cname="$(docker ps -a --format '{{.Names}}' | grep -E -- "-${name}\$" | head -1 || true)"

  # First deploy of a service that has never run here.
  #
  # The clone-the-running-container path below cannot bootstrap one: there is no
  # predecessor to clone. Three services need it and they need different things,
  # so each first-create is written out rather than inferred.
  #
  #   pay-frontend    application plane only. No secret, no database URL, no Core
  #                   credential — only the public gateway origin it reads.
  #   admin-frontend  the same shape: a browser app that talks to admin-api
  #                   through the edge, never directly to a database.
  #   admin-api       data plane, and the only one here that needs credentials.
  #                   It reads the same database as core-api and calls Core's
  #                   internal routes, so it gets the same file-only mounts and
  #                   the same in-process export the other services use — never
  #                   a credential in `-e`, which docker inspect would print.
  if [ -z "$cname" ] && { [ "$name" = "pay-frontend" ] || [ "$name" = "admin-frontend" ] || [ "$name" = "admin-api" ]; }; then
    local proj appnet datanet
    proj="$(docker ps --format '{{.Names}}' | grep -oE '^bzsandbox-[0-9]+-[0-9]+-[0-9]+' | head -1)"
    [ -n "$proj" ] || die "no bootstrapped Sandbox project found"
    appnet="$(docker network ls --format '{{.Name}}' | grep -E '^bzsb-app-' | head -1)"
    [ -n "$appnet" ] || die "no Sandbox application network found"
    cname="${proj}-${name}"

    if [ "$name" = "admin-api" ]; then
      # Where the credential files live is read from a service that is already
      # running, not from the bootstrap state file.
      #
      # That state file lives under /tmp and does not survive a reboot; every
      # other deploy-one works anyway because it clones a container rather than
      # reading it. Deriving the paths from core-api-staging's own mounts is
      # both more robust and more correct: admin-api gets the same db_url the
      # rest of the stack has, by construction, instead of a second copy that
      # could drift from it.
      local core_c secret_dir
      core_c="$(docker ps --format '{{.Names}}' | grep -E -- '-core-api-staging$' | head -1)"
      [ -n "$core_c" ] || die "core-api-staging is not running — cannot locate the Sandbox credential files"
      secret_dir="$(docker inspect "$core_c" --format '{{range .HostConfig.Binds}}{{println .}}{{end}}' \
        | grep '/run/secrets/db_url:' | head -1 | sed 's#/db_url:.*##')"
      [ -n "$secret_dir" ] && [ -d "$secret_dir" ] || die "cannot locate the Sandbox credential directory"
      DBURL_FILE="$secret_dir/db_url"
      CIK_FILE="$secret_dir/core_internal_key"
      ADMINJWT_FILE="$secret_dir/admin_jwt_secret"
      # The transactional-mail credential, shared with developer-api.
      #
      # BANZADMIN needs to send: an operator password reset is the only way an
      # administrator can ever set their own password, and the first one has no
      # session to request it with. Without a mailer the reset link is returned
      # in the API response instead — which turns a password into something that
      # travels through whoever happened to call the endpoint.
      #
      # Copied file-to-file on the host so the value is never an argument, never
      # in `-e`, and never printed. Absent, admin-api still starts and simply
      # cannot send, which it warns about.
      RESEND_FILE="$secret_dir/resend_api_key"
      if [ ! -s "$RESEND_FILE" ]; then
        local dev_c
        dev_c="$(docker ps --format '{{.Names}}' | grep -E -- '-developer-api$' | head -1)"
        if [ -n "$dev_c" ]; then
          docker inspect "$dev_c" --format '{{range .Config.Env}}{{println .}}{{end}}' \
            | sed -n 's/^RESEND_API_KEY=//p' | head -1 | tr -d '\r\n' > "$RESEND_FILE"
          chmod 0644 "$RESEND_FILE"
        fi
        [ -s "$RESEND_FILE" ] && echo "  resend_api_key provisioned from developer-api" \
                              || echo "  resend_api_key NOT available — BANZADMIN cannot send mail"
      fi
      datanet="$(docker network ls --format '{{.Name}}' | grep -E '^bzsb-data-' | head -1)"
      [ -n "$datanet" ] || die "no Sandbox data network found"
      # The admin JWT signing key. Preserved across applies like every other
      # credential — regenerating it would sign every operator out and, worse,
      # would do it silently at the next deploy.
      assert_secret_modes "$secret_dir"
      keep_or_mint "$ADMINJWT_FILE" admin_jwt_secret
      echo "  $name first create on $datanet + $appnet (file-only credentials)"
      docker create --name "$cname" --network "$datanet" \
        --security-opt "no-new-privileges:true" \
        --label "$LABEL.service=$name" \
        -v "$DBURL_FILE:/run/secrets/db_url:ro" \
        -v "$CIK_FILE:/run/secrets/core_internal_key:ro" \
        -v "$ADMINJWT_FILE:/run/secrets/admin_jwt_secret:ro" \
        -v "$RESEND_FILE:/run/secrets/resend_api_key:ro" \
        -e "ADMIN_API_PORT=$port" \
        -e "ENVIRONMENT=SANDBOX" \
        -e "EMAIL_PROVIDER=resend" \
        -e "EMAIL_DRY_RUN=false" \
        -e "EMAIL_FROM_NAME=Banzami" \
        -e "EMAIL_FROM_ADDRESS=contact@banzami.com" \
        -e "EMAIL_NOREPLY_NAME=Banzami" \
        -e "EMAIL_NOREPLY_ADDRESS=noreply@banzami.com" \
        -e "EMAIL_REPLY_TO=contact@banzami.com" \
        -e "ADMIN_BASE_URL=https://admin.banzami.com" \
        -e "CORE_API_URL=http://${proj}-core-api-staging:8081" \
        -e "GATEWAY_STAGING_INTERNAL_URL=http://${proj}-api-gateway-staging:8080" \
        --entrypoint sh "$tag" -c 'export DATABASE_URL="$(cat /run/secrets/db_url)"; export INTERNAL_API_KEY="$(cat /run/secrets/core_internal_key)"; export STAGING_INTERNAL_API_KEY="$INTERNAL_API_KEY"; export ADMIN_JWT_SECRET="$(cat /run/secrets/admin_jwt_secret)"; [ -s /run/secrets/resend_api_key ] && export RESEND_API_KEY="$(cat /run/secrets/resend_api_key)"; exec admin-api' >/dev/null 2>&1 \
        || { echo "  $name first create FAIL"; return 1; }
      docker network connect "$appnet" "$cname" >/dev/null 2>&1 || true
      # Outbound internet. The data and application networks are both internal:
      # bzsb-egress is the only one that is not, and api-gateway and
      # developer-api are on it because they call Resend and deliver webhooks.
      #
      # admin-api was not, and the symptom was not a startup failure — it was a
      # DNS error inside a send that had already reported success. The console
      # sends the operator activation and password-reset mail; without egress it
      # can create an operator who can never activate.
      local egressnet
      egressnet="$(docker network ls --format '{{.Name}}' | grep -E '^bzsb-egress$' | head -1)"
      [ -n "$egressnet" ] && docker network connect "$egressnet" "$cname" >/dev/null 2>&1 || true
      docker start "$cname" >/dev/null 2>&1 || { echo "  $name first start FAIL"; return 1; }
    else
      local extra_env=()
      if [ "$name" = "pay-frontend" ]; then
        extra_env=(-e NEXT_PUBLIC_GATEWAY_URL="${PAY_GATEWAY_URL:-https://sandbox-api.banzami.com}"
                   -e GATEWAY_INTERNAL_URL="http://${proj}-api-gateway-staging:8080")
      else
        # The browser calls admin-api through the same origin the console is
        # served on, so the bundle needs no host of its own.
        extra_env=(-e NEXT_PUBLIC_ADMIN_API_URL="${ADMIN_API_PUBLIC_URL:-https://admin.banzami.com/api}")
      fi
      echo "  $name first create on $appnet (application plane only, no secrets)"
      docker run -d --name "$cname" --network "$appnet" \
        --security-opt "no-new-privileges:true" \
        --label "$LABEL.service=$name" \
        "${extra_env[@]}" \
        -e PORT="$port" -e HOSTNAME=0.0.0.0 \
        "$tag" >/dev/null 2>&1 || { echo "  $name first create FAIL"; return 1; }
    fi

    local c=0 st
    while :; do st="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}nohc{{end}}' "$cname" 2>/dev/null)"
      case "$st" in healthy) echo "  $name deployed_and_healthy PASS"; return 0 ;; esac
      [ "$st" = nohc ] && { docker exec "$cname" true >/dev/null 2>&1 && { echo "  $name deployed PASS (no healthcheck)"; return 0; }; }
      c=$((c+1)); [ "$c" -gt 45 ] && { echo "  $name first create FAIL (health timeout)"; docker logs --tail 20 "$cname" 2>&1 | sed 's/^/    /'; return 1; }; sleep 2
    done
  fi

  [ -n "$cname" ] || die "no running $name container to redeploy (run a full gated apply first)"
  # The mounts are cloned from the predecessor, so their modes are whatever the
  # host has now — assert them before a container is built around them.
  # The `|| true` is on the ASSIGNMENT, and that is the whole point.
  #
  # The script runs under `set -euo pipefail`. A service with no secret mounts —
  # pay-frontend, admin-frontend — makes the grep match nothing, the pipeline
  # exits non-zero, and `sd="$(...)"` is itself the failing command, so the
  # deploy aborts before it starts. Two admin-frontend deploys reported
  # "deployed_and_healthy FAIL" and rolled back an image that was fine, which
  # sent me looking at the image rather than at this line.
  local sd
  sd="$(docker inspect "$cname" --format '{{range .HostConfig.Binds}}{{println .}}{{end}}' 2>/dev/null \
        | grep '/run/secrets/' | head -1 | sed 's#/[^/]*:/run/secrets/.*##' || true)"
  if [ -n "$sd" ]; then assert_secret_modes "$sd" || true; fi

  local prev pf; prev="$(docker inspect -f '{{.Config.Image}}' "$cname" 2>/dev/null || true)"; pf="/tmp/.banzami-prev-img-$name"
  if [ "$rollback" = "--rollback" ]; then tag="$(cat "$pf" 2>/dev/null || echo "$tag")"; else [ -n "$prev" ] && printf '%s' "$prev" > "$pf" || true; fi
  # clone config from the running container
  local nets; mapfile -t nets < <(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{println $k}}{{end}}' "$cname")
  local run=(docker run -d --name "$cname" --security-opt "no-new-privileges:true" --network "${nets[0]}")
  # Carry the restart policy across. Everything else about the container is
  # cloned from its predecessor and this was not, so every deploy silently reset
  # it to "no" — an intent set once, lost the next time anyone deployed. The
  # host attestation caught it on pay-frontend, which is declared
  # unless-stopped and would not have come back after a host reboot.
  local rp; rp="$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$cname" 2>/dev/null || true)"
  case "$rp" in ''|no) : ;; *) run+=(--restart "$rp") ;; esac
  [ "$name" = developer-api ] && run+=(--network-alias developer-api)
  local x; while IFS= read -r x; do [ -n "$x" ] && run+=(-v "$x"); done < <(docker inspect -f '{{range .HostConfig.Binds}}{{println .}}{{end}}' "$cname")
  # A NEW secret cannot arrive by cloning: the loop above copies the
  # predecessor's mounts, and the predecessor does not have one. Added
  # explicitly, and only for a service that already holds credentials, so an
  # application-plane container stays credential-free.
  if [ -n "$sd" ] && [ -d "$sd" ]; then
    ensure_proof_signing_key "$sd"
    printf '%s\n' "${run[@]}" | grep -q '/run/secrets/bzm_proof_signing_key' \
      || run+=(-v "$sd/bzm_proof_signing_key:/run/secrets/bzm_proof_signing_key:ro")
  fi
  # Clone the previous container's env EXCEPT anything the new image is the
  # authority on. BANZAMI_BUILD_COMMIT is baked into each image by the build
  # (ARG -> ENV); re-applying the previous container's value as an explicit -e
  # shadows the new image's ENV, so the deployed build would report whichever
  # commit happened to be running when the env was first cloned — and would keep
  # reporting it through every future deploy.
  #
  # Caught by the runtime gate immediately after a deploy: the container was
  # created from image :9c2d0f428fec, whose ENV says 9c2d0f428fec, while /readyz
  # answered 4a924e764024. Build identity that silently freezes is worse than no
  # build identity, because it looks like an answer.
  local owned; owned="$(release_config_env "$name")"
  while IFS= read -r x; do
    case "$x" in BANZAMI_BUILD_COMMIT=*) continue ;; esac
    # A release-owned name is re-applied below from the release, not carried
    # over from the container that happened to be running.
    if [ -n "$owned" ] && printf '%s\n' "$owned" | grep -q "^${x%%=*}="; then continue; fi
    [ -n "$x" ] && run+=(-e "$x")
  done < <(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$cname")
  while IFS= read -r x; do [ -n "$x" ] && run+=(-e "$x"); done < <(printf '%s\n' "$owned")
  # reconstruct the file-only-secret entrypoint (secrets exported in-process, never -e).
  #
  # core_internal_key appears twice, under two names. Core gates its refund group
  # on one shared service credential, so the Gateway's CORE_INTERNAL_KEY and
  # developer-api's CORE_REFUND_KEY are the same value — but each service names
  # the variable for what it authorises there, so reading either one says what
  # that service is allowed to do rather than where the secret came from. A
  # service that does not read a name simply ignores it.
  #
  # admin_jwt_secret and resend_api_key were missing from this list, and the
  # consequence was invisible: the FIRST create of admin-api mounted and
  # exported them, the first REDEPLOY cloned the mounts and rebuilt the
  # entrypoint from this list, and the console came back up with operator login
  # disabled (503) and no mailer. Nothing failed; the service was healthy and
  # nobody could sign in.
  #
  # INTERNAL_API_KEY and STAGING_INTERNAL_API_KEY were the same omission, one
  # layer down, and it stayed hidden for the same reason. admin-api's first
  # create exports both from core_internal_key; this list did not, so every
  # redeploy dropped them. The Gateway then ran InternalAuth("") — which answers
  # 503 "internal API is not configured" to the whole internal route group — and
  # admin-api, which cannot tell that apart from an unreachable upstream, turned
  # it into 502. The visible symptom was the operator console's Business
  # applications and KYB review being permanently unavailable, with both
  # services reporting healthy.
  #
  # One value on both sides, named for what it authorises at each end: the
  # Gateway reads INTERNAL_API_KEY to decide whether to accept an internal call,
  # admin-api reads STAGING_INTERNAL_API_KEY to decide what to send.
  local ep='for s in db_url:DATABASE_URL jwt_secret:JWT_SECRET core_internal_key:CORE_INTERNAL_KEY core_internal_key:CORE_REFUND_KEY api_key_pepper:API_KEY_PEPPER developer_internal_key:DEVELOPER_INTERNAL_KEY core_payee_validation_key:CORE_PAYEE_VALIDATION_KEY session_secret:SESSION_SECRET otp_pepper:OTP_PEPPER admin_jwt_secret:ADMIN_JWT_SECRET resend_api_key:RESEND_API_KEY core_internal_key:INTERNAL_API_KEY core_internal_key:STAGING_INTERNAL_API_KEY bzm_proof_signing_key:BZM_PROOF_SIGNING_KEY; do f="/run/secrets/${s%%:*}"; v="${s##*:}"; [ -f "$f" ] && export "$v"="$(cat "$f")"; done; exec '"$bin"
  docker rm -f "$cname" >/dev/null 2>&1 || true   # single-service swap (nothing else pruned)
  "${run[@]}" --entrypoint sh "$tag" -c "$ep" >/dev/null 2>&1 || { echo "  $name docker run FAIL"; return 1; }
  local i; for i in "${nets[@]:1}"; do docker network connect "$i" "$cname" >/dev/null 2>&1 || true; done
  local k=0 st; while :; do st="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}nohc{{end}}' "$cname" 2>/dev/null)"
    [ "$st" = healthy ] && { echo "  $name deployed_and_healthy PASS"; return 0; }
    [ "$st" = nohc ] && { docker exec "$cname" true >/dev/null 2>&1 && { echo "  $name deployed PASS (no healthcheck)"; return 0; }; }
    k=$((k+1)); [ "$k" -gt 45 ] && { echo "  $name deployed_and_healthy FAIL (health timeout)"; return 1; }; sleep 2
  done
}

case "${1:-}" in
  plan) cmd_plan ;; apply) cmd_apply ;; verify) cmd_verify ;; clean) cmd_clean ;;
  deploy-one) shift; cmd_deploy_one "$@" ;;
  *) die "usage: sandbox-deploy.sh {plan|apply|verify|clean|deploy-one <name> <tag> [--rollback]}" ;;
esac
