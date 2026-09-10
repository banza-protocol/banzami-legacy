#!/usr/bin/env bash
# Pricing follows the owner's assigned policy — proved on the deployed Sandbox.
#
# Three states that must stay distinguishable, and used not to be:
#
#   an owner assigned an explicit 0-bps policy settles, at zero
#   an owner assigned a 200-bps policy settles, at 200 bps
#   an owner with NO policy is refused, not settled for free
#
# The last one is the defect. business_category came from the request, chose
# among the operator's rates, and an unmatched category cost nothing — so the
# Developer-key path, which sent none at all, settled everything free.
#
# Every attempt below also tries to buy a cheaper rate: through the legacy body
# field, and through a wallet's own metadata. None may move the fee.
set -uo pipefail
. "$(cd "$(dirname "$0")" && pwd)/lib/e2e-run.sh"
e2e_begin

GW=$(docker ps  --format '{{.Names}}' | grep api-gateway-staging | head -1)
PUB=$(docker ps --format '{{.Names}}' | grep public-api-staging  | head -1)
CORE=$(docker ps --format '{{.Names}}'| grep core-api-staging    | head -1)
DEV=$(docker ps --format '{{.Names}}' | grep developer-api       | head -1)
PG=$(docker ps  --format '{{.Names}}' | grep postgres | grep bzsandbox | head -1)
DEVINT=$(docker exec "$DEV" sh -c 'cat /run/secrets/developer_internal_key')
JWTSEC=$(docker exec "$GW" sh -c 'cat /run/secrets/jwt_secret')
PW=$(docker exec "$CORE" sh -c 'cat /run/secrets/db_url' | sed -E 's#.*://[^:]+:([^@]+)@.*#\1#')
# Trimmed at the source. An id with a trailing newline interpolated into a
# quoted SQL literal produces "invalid input syntax for type uuid" with the
# opening quote visibly unmatched — which is what the first run of this harness
# reported, and it read like a product failure.
# -q as well as -At: without it psql prints the command tag ("INSERT 0 1")
# after a RETURNING row, and trimming newlines concatenates the two into an id
# that is not an id. The first run recorded a merchant under
# "<uuid>INSERT 0 1", which then failed its own cleanup.
q(){ docker exec -e PGPASSWORD="$PW" "$PG" psql -q -U bl_app_runtime -d banzami_staging -At -c "$1" 2>/dev/null | tr -d '\r\n'; }
qerr(){ docker exec -e PGPASSWORD="$PW" "$PG" psql -q -U bl_app_runtime -d banzami_staging -At -c "$1" 2>&1 | tr '\n' ' '; }

PASS=0; FAIL=0
chk(){ if [ "$2" = "$3" ]; then echo "  $1 PASS ($2)"; PASS=$((PASS+1)); else echo "  $1 FAIL (got '$2' want '$3')"; FAIL=$((FAIL+1)); fi; }
LAST=""; CODE=""
call(){ local ct="$1" port="$2" m="$3" p="$4" bd="$5" au="$6"
  local a=(curl -s -w $'\n%{http_code}' -X "$m" "http://localhost:$port$p")
  [ "$au" != "-" ] && a+=(-H "Authorization: Bearer $au")
  local r
  if [ "$bd" = "-" ]; then r=$(docker exec "$ct" "${a[@]}" 2>/dev/null)
  else a+=(-H "Content-Type: application/json" --data @-); r=$(printf '%s' "$bd" | docker exec -i "$ct" "${a[@]}" 2>/dev/null); fi
  CODE=$(printf '%s' "$r" | tail -n1); LAST=$(printf '%s' "$r" | sed '$d'); }
jget(){ printf '%s' "$LAST" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{process.stdout.write(String(JSON.parse(s)["'"$1"'"]??""))}catch(e){}})'; }
errcode(){ printf '%s' "$LAST" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{process.stdout.write(String(JSON.parse(s).error?.code??""))}catch(e){}})'; }

R="${RANDOM}${RANDOM}"
SC='["identity:read","payment_sessions:read","payment_sessions:write","wallet_accounts:read","wallet_accounts:create","application_settlements:write"]'
ACTOR="11111111-2222-4333-8444-555555555555"

# ── an owner priced by the explicit zero, and one by 200 bps ─────────────────
mkproject(){ # $1 = profile code | prints project_id|key|merchant_id
  call "$DEV" 8086 POST /internal/v1/fixture-projects "{\"name\":\"pricing-$1-$R\",\"created_by\":\"$ACTOR\"}" "-"
  local pj; pj=$(printf '%s' "$LAST" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{process.stdout.write(JSON.parse(s).project_id||"")}catch(e){}})')
  :
}
echo "### two owners, two policies"
# Reuse the deployed self-service path: it is what an external developer gets.
ZERO_M=$(q "select m.id from merchants m join pricing_profiles p on p.id=m.pricing_profile_id
             where m.status='ACTIVE' and p.code='sandbox-default' order by m.created_at desc limit 1")
PAID_M=$(q "select m.id from merchants m join pricing_profiles p on p.id=m.pricing_profile_id
             where m.status='ACTIVE' and p.code='sandbox-reference' limit 1")
chk ZERO_OWNER_FOUND "$([ -n "$ZERO_M" ] && echo yes)" yes
chk PAID_OWNER_FOUND "$([ -n "$PAID_M" ] && echo yes)" yes
[ -n "$ZERO_M" ] && [ -n "$PAID_M" ] || { echo "no owners to price"; exit 1; }

echo "### the settlement rate each owner resolves to"
# pricing_operation='SETTLEMENT', named.
#
# Without it this joins every enabled rule on the profile — which since the
# payout rule (ADR-031, 0.75%) is two rows — and the shell concatenates them:
# the zero-rated owner reported "750" and the 200-bps owner "75200". Neither is
# a rate; both are two rates stuck together, and the assertion was comparing
# them against a settlement number.
#
# This is the exact confusion the pricing model itself removed: before
# `pricing_operation` a settlement and a payout were the same thing to the
# resolver, so no rule could price one without pricing the other. A test that
# reintroduces it cannot check that it is gone.
settlement_rate() { # <merchant-id>
  q "select r.rate_bps from merchants m join pricing_profiles p on p.id=m.pricing_profile_id
      join pricing_rules r on r.pricing_profile=p.code and r.environment=p.environment
                          and r.enabled and r.pricing_operation='SETTLEMENT'
     where m.id='$1'"
}
ZRATE=$(settlement_rate "$ZERO_M")
PRATE=$(settlement_rate "$PAID_M")
chk EXPLICIT_ZERO_RATE "$ZRATE" "0"
chk GENERIC_200_RATE   "$PRATE" "200"

echo "### an owner with no policy is refused, not settled free"
# A controlled owner deliberately left unpriced. It is created and unassigned
# here on purpose: this is the state the old code turned into a free settlement.
UNPRICED=$(q "insert into merchants (id, name, email, status)
              values (gen_random_uuid(), 'pricing-unpriced-$R', 'unpriced-$R@projects.banzami.test', 'ACTIVE')
              returning id")
e2e_own merchant "$UNPRICED"
chk UNPRICED_OWNER_CREATED "$([ -n "$UNPRICED" ] && echo yes)" yes
NOPROFILE=$(q "select coalesce(pricing_profile_id::text,'none') from merchants where id='$UNPRICED'")
chk UNPRICED_HAS_NO_POLICY "$NOPROFILE" "none"

echo "### the environment guard refuses a non-Sandbox policy"
# Create the probe, test it and destroy it in ONE statement.
#
# This used to be three: insert, attempt, delete. Any exit between the first and
# the last leaked an enabled LIVE pricing profile into the deployed database,
# and two of them are sitting there now from a run that did not reach its third
# statement. Nothing bad came of it — LIVE is fail-closed and they were assigned
# to no one — but an authority-shaped object in a financial table is not
# something a test should be able to leave behind.
#
# The run manifest is the usual answer and does not fit: it retires objects over
# HTTP, and a pricing profile has no retirement route. So the window is closed
# instead of cleaned up after. A DO block is a single statement, so either it
# completes — probe deleted — or it aborts entirely and the insert rolls back
# with it. There is no state in which the probe survives.
OUT=$(qerr "DO \$\$
DECLARE gid uuid; msg text;
BEGIN
  INSERT INTO pricing_profiles (id, code, name, enabled, environment)
  VALUES (gen_random_uuid(), 'live-probe-$R', 'probe', true, 'LIVE')
  RETURNING id INTO gid;
  BEGIN
    UPDATE merchants SET pricing_profile_id = gid WHERE id = '$UNPRICED';
    msg := 'ALLOWED';
  EXCEPTION WHEN others THEN msg := 'REFUSED ' || SQLERRM;
  END;
  DELETE FROM pricing_profiles WHERE id = gid;
  RAISE NOTICE '%', msg;
END \$\$;")
case "$OUT" in
  *REFUSED*Sandbox-only*) chk LIVE_PROFILE_REFUSED refused refused ;;
  *ALLOWED*)              chk LIVE_PROFILE_REFUSED allowed refused ;;
  *)                      chk LIVE_PROFILE_REFUSED "inconclusive($OUT)" refused ;;
esac
# And prove the probe is gone, so a future regression in the block itself cannot
# quietly reintroduce the leak this was written to stop.
chk LIVE_PROBE_LEFT_NOTHING "$(q "SELECT COUNT(*) FROM pricing_profiles WHERE code = 'live-probe-$R'")" "0"

echo "### descriptive text cannot move a rate"
BEFORE=$(q "select p.code from merchants m join pricing_profiles p on p.id=m.pricing_profile_id where m.id='$ZERO_M'")
q "update merchants set name = name || ' doação crowd vaquinha' where id='$ZERO_M'" >/dev/null
AFTER=$(q "select p.code from merchants m join pricing_profiles p on p.id=m.pricing_profile_id where m.id='$ZERO_M'")
q "update merchants set name = replace(name, ' doação crowd vaquinha', '') where id='$ZERO_M'" >/dev/null
chk LABEL_CANNOT_MOVE_PRICING "$AFTER" "$BEFORE"

echo
echo "PRICING_AUTHORITY_E2E: PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
