#!/usr/bin/env bash
# The operator's rate, charged where the operator actually charges it — proved
# on the deployed Sandbox.
#
# THE MODEL THIS TESTS
#
# The transfer primitive is neutral: it carries merchant payments and P2P alike,
# so pricing inside it would charge people for sending money to each other. An
# incoming payment or donation therefore credits the merchant or campaign wallet
# GROSS. The operator's rate is resolved one step later, at a fee-bearing
# operation — here, an application settlement.
#
# So the arithmetic under test is NOT "a 100 000 donation credits 98 000". It is:
#
#   wallet holds        100 000   (gross, from the payment leg)
#   settle              100 000
#   assigned profile    sandbox-reference
#   rate                    200 bps
#   fee                   2 000
#   net to beneficiary   98 000
#
# The gross-credit half is proved separately by doa-public-donation-e2e.sh,
# which asserts the campaign account is credited the FULL link amount.
#
# WHY THIS EXISTS ALONGSIDE economic-model-smoke.sh
#
# That smoke proves the model broadly — one settlement per profile, plus the
# refusals and the ledger invariants. This harness proves ONE thing that a
# single settlement cannot: that two different owners on the same plan are
# charged identically. The differential is the assertion, and it needs two runs
# to exist at all.
#
# AND ECONOMIC PARITY
#
# The same settlement runs twice: once for an ordinary controlled owner and once
# for a second owner standing in for a reference application. Both carry the
# same generic profile, so both must produce the same rate, the same fee and the
# same net. The differential must be exactly zero — no tenant is special.
#
# The profile is `sandbox-reference`. It replaced a profile whose name carried
# a vertical and a rate; the rename happened before either shipped, in
# the canonical pricing model. The old code named a business vertical and a rate; a profile
# names a commercial policy, and its per-operation rules name what that policy
# charges. The rate here is unchanged — only the identity is stable now.
#
# THE CONTRACT THIS SPEAKS
#
# It used to POST source_wallet_id / beneficiary_wallet_id /
# application_fee_wallet_id. That shape has no route — /v1/application-settlements
# is served by CreateBusiness, which takes a segregated ACCOUNT id and two @banza
# names. Every run answered 400, and every economic assertion below it was
# reported as a settlement that produced no fee. The harness that exists to prove
# the operator charges its rate had been proving nothing for as long as the two
# shapes disagreed.
#
# The parties are now named the way the contract names them, so what is asserted
# is what an integrator actually gets:
#
#   source        a CAMPAIGN account the caller owns, credited by a real payment
#   beneficiary   a @banza — a consumer, because a settlement pays a person
#   fee dest      the caller's OWN @banza; ADR-028 refuses anyone else's
#
# And the gross is put there by a payer paying a payment session, not by an
# admin credit to the wallet's primary account. A settlement refuses a PRIMARY
# source, so funding one would have been funding the wrong account — and the
# gross-credit leg is half the arithmetic this file describes.
set -uo pipefail

GW=$(docker ps  --format '{{.Names}}' | grep api-gateway-staging | head -1)
PUB=$(docker ps --format '{{.Names}}' | grep public-api-staging  | head -1)
CORE=$(docker ps --format '{{.Names}}'| grep core-api-staging    | head -1)
PG=$(docker ps  --format '{{.Names}}' | grep postgres | grep bzsandbox | head -1)
[ -n "$CORE" ] && [ -n "$GW" ] || { echo "NO_CONTAINERS"; exit 1; }
PW=$(docker exec "$CORE" sh -c 'cat /run/secrets/db_url' | sed -E 's#.*://[^:]+:([^@]+)@.*#\1#')
JWTSEC=$(docker exec "$GW" sh -c 'cat /run/secrets/jwt_secret')
[ -n "$JWTSEC" ] || { echo "NO_JWT_SECRET"; exit 1; }

. "$(cd "$(dirname "$0")" && pwd)/lib/e2e-run.sh"
e2e_begin

q(){ docker exec -e PGPASSWORD="$PW" "$PG" psql -q -U bl_app_runtime -d banzami_staging -At -c "$1" 2>/dev/null | tr -d '\r\n'; }

PASS=0; FAIL=0
chk(){ if [ "$2" = "$3" ]; then echo "  $1 PASS ($2)"; PASS=$((PASS+1));
       else echo "  $1 FAIL (got '$2' want '$3')"; FAIL=$((FAIL+1)); fi; }

LAST=""; CODE=""
call(){ local ct="$1" port="$2" m="$3" p="$4" bd="$5" au="${6:--}"
  local a=(curl -s -w $'\n%{http_code}' -X "$m" "http://localhost:$port$p")
  [ "$au" != "-" ] && a+=(-H "Authorization: Bearer $au")
  local r
  if [ "$bd" = "-" ]; then r=$(docker exec "$ct" "${a[@]}" 2>/dev/null)
  else a+=(-H "Content-Type: application/json" --data @-); r=$(printf '%s' "$bd" | docker exec -i "$ct" "${a[@]}" 2>/dev/null); fi
  CODE=$(printf '%s' "$r" | tail -n1); LAST=$(printf '%s' "$r" | sed '$d'); }
jget(){ printf '%s' "$LAST" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{process.stdout.write(String(JSON.parse(s)["'"$1"'"]??""))}catch(e){}})'; }
mint(){ SECRET="$JWTSEC" K=merchant_id V="$1" node -e 'const c=require("crypto");const b=o=>Buffer.from(typeof o==="string"?o:JSON.stringify(o)).toString("base64url");const n=Math.floor(Date.now()/1000);const cl={scopes:["*"],environment:"SANDBOX",iat:n,exp:n+900};cl[process.env.K]=process.env.V;const h=b({alg:"HS256",typ:"JWT"}),p=b(cl);process.stdout.write(h+"."+p+"."+c.createHmac("sha256",process.env.SECRET).update(h+"."+p).digest("base64url"));'; }
mintc(){ SECRET="$JWTSEC" K=customer_id V="$1" node -e 'const c=require("crypto");const b=o=>Buffer.from(typeof o==="string"?o:JSON.stringify(o)).toString("base64url");const n=Math.floor(Date.now()/1000);const cl={scopes:["*"],environment:"SANDBOX",iat:n,exp:n+900};cl[process.env.K]=process.env.V;const h=b({alg:"HS256",typ:"JWT"}),p=b(cl);process.stdout.write(h+"."+p+"."+c.createHmac("sha256",process.env.SECRET).update(h+"."+p).digest("base64url"));'; }

wbal(){ q "SELECT COALESCE(SUM(CASE WHEN entry_type='CREDIT' THEN amount_minor ELSE -amount_minor END),0)
             FROM ledger_entries WHERE account_id=(SELECT available_account_id FROM wallets WHERE id='$1')"; }
# A segregated account's balance, read from the ledger account it maps to.
abal(){ q "SELECT COALESCE(SUM(CASE WHEN e.entry_type='CREDIT' THEN e.amount_minor ELSE -e.amount_minor END),0)
             FROM ledger_entries e
            WHERE e.account_id=(SELECT account_id FROM wallet_accounts WHERE id='$1')"; }
# A consumer's own available balance, by @banza.
hbal(){ q "SELECT COALESCE(SUM(CASE WHEN e.entry_type='CREDIT' THEN e.amount_minor ELSE -e.amount_minor END),0)
             FROM ledger_entries e
            WHERE e.account_id=(SELECT w.available_account_id FROM consumer_wallets w
                                  JOIN consumers c ON c.id=w.consumer_id
                                 WHERE c.handle='$1' AND w.status='ACTIVE' LIMIT 1)"; }
unbalanced(){ q "SELECT COUNT(*) FROM (SELECT p.id FROM ledger_postings p
                    JOIN ledger_entries e ON e.posting_id=p.id GROUP BY p.id
                   HAVING SUM(CASE e.entry_type WHEN 'DEBIT' THEN -e.amount_minor ELSE e.amount_minor END) <> 0) x"; }

R="${RANDOM}${RANDOM}"
GROSS="${GROSS:-100000}"
RATE_BPS=200
EXPECTED_FEE=$(( GROSS * RATE_BPS / 10000 ))
EXPECTED_NET=$(( GROSS - EXPECTED_FEE ))

echo "### the arithmetic under test: gross $GROSS @ ${RATE_BPS}bps -> fee $EXPECTED_FEE, net $EXPECTED_NET"
chk LEDGER_SOUND_BEFORE "$(unbalanced)" "0"

# ── one payer, one beneficiary, shared by both cases ────────────────────────
# Shared deliberately: the parity assertion compares two owners, and giving each
# its own payer would let a funding difference masquerade as an economic one.
onboard(){ # $1 = handle-ish suffix -> prints consumer_id
  local ph="+2449${R:0:4}$1" h="se${R:0:4}$1" sid
  call "$PUB" 8083 POST /v1/consumer/onboarding/start \
    "{\"phone_number\":\"$ph\",\"currency\":\"AOA\",\"otp_plaintext_for_test\":\"123456\"}" -
  sid=$(jget session_id)
  call "$PUB" 8083 POST /v1/consumer/onboarding/verify-otp "{\"session_id\":\"$sid\",\"otp_code\":\"123456\"}" -
  call "$PUB" 8083 POST /v1/consumer/onboarding/complete \
    "{\"session_id\":\"$sid\",\"banza_handle\":\"$h\",\"pin\":\"1234\"}" -
  printf '%s|%s' "$(jget consumer_id)" "$h"
}

IFS='|' read -r PAYER_ID PAYER_HANDLE <<<"$(onboard 71)"
PAYER_JWT=$(mintc "$PAYER_ID")
call "$GW" 8080 POST /v1/compliance/customers/verify \
  "{\"full_name\":\"SETTLEMENT ECONOMICS\",\"document_type\":\"BILHETE_DE_IDENTIDADE\",\"document_number\":\"SE$R\",\"date_of_birth\":\"1990-01-01\",\"requested_level\":\"BASIC\"}" "$PAYER_JWT"
# Enough for both cases plus headroom; a short payer reads as a settlement fault.
call "$PUB" 8083 POST /v1/sandbox/fund "{\"amount_minor\":$(( GROSS * 3 )),\"currency\":\"AOA\"}" "$PAYER_JWT"
chk PAYER_FUNDED "$([ "$(hbal "$PAYER_HANDLE")" -ge "$(( GROSS * 2 ))" ] && echo yes)" yes

IFS='|' read -r BENEFICIARY_ID BENEFICIARY_HANDLE <<<"$(onboard 72)"
chk BENEFICIARY_READY "$([ -n "$BENEFICIARY_HANDLE" ] && echo yes)" yes
BENEFICIARY_START=$(hbal "$BENEFICIARY_HANDLE")

# An owner on the generic 200-bps profile, with a wallet holding gross.
mkowner(){ # $1 = label -> prints merchant|source_wallet|beneficiary_wallet
  local mid sw bw
  # An APPLICATION account with approved KYB. Only APPLICATION/PLATFORM accounts
  # may receive an application fee, and only KYB-approved ones — a plain MERCHANT
  # is refused at the fee destination, which has nothing to do with pricing and
  # looks exactly like it does. The identity is generated by the database:
  # naming owners after the case collided the moment two cases ran in one go.
  mid=$(q "INSERT INTO merchants (id, name, email, status, business_account_type)
           VALUES (gen_random_uuid(), 'settle-$1-$R',
                   'settle-' || gen_random_uuid() || '@projects.banzami.test', 'ACTIVE', 'APPLICATION')
           RETURNING id")
  [ -n "$mid" ] || return 1
  e2e_own merchant "$mid"
  q "INSERT INTO merchant_compliance (merchant_id, kyb_status, aml_status)
     VALUES ('$mid','APPROVED','APPROVED')
     ON CONFLICT (merchant_id) DO UPDATE SET kyb_status='APPROVED', aml_status='APPROVED'" >/dev/null
  call "$CORE" 8081 PUT "/internal/v1/merchants/$mid/pricing-profile" '{"profile_code":"sandbox-reference"}'
  call "$CORE" 8081 POST /internal/v1/wallets "{\"merchant_id\":\"$mid\",\"currency\":\"AOA\"}"; sw=$(jget id)
  # The beneficiary is a SEPARATE owner. A merchant holds exactly one wallet per
  # currency, so asking the same one for a second wallet fails on
  # wallets_merchant_id_currency_key and leaves the beneficiary empty — which
  # then reads as a settlement failure rather than as a fixture that cannot
  # express what it is describing.
  # The settling owner must be NAMEABLE. ADR-028 requires the application fee to
  # land in the caller's OWN business account, and the contract names it by
  # @banza — so without a handle the owner cannot be its own fee destination and
  # the settlement is refused for a reason that has nothing to do with pricing.
  local ownh
  ownh=$(printf 's%s' "$(printf '%s' "$mid" | tr -d '-' | cut -c1-11)" | tr 'A-Z' 'a-z')
  q "INSERT INTO handle_registry (handle, owner_type, owner_id, created_at)
     VALUES ('$ownh','MERCHANT','$mid', now()) ON CONFLICT (handle) DO NOTHING" >/dev/null

  # A segregated CAMPAIGN account, because settlement moves funds OUT of one and
  # refuses the PRIMARY. That refusal is the segregation working, so funding the
  # primary would have been funding the wrong account.
  local jwt acct; jwt=$(mint "$mid")
  call "$GW" 8080 POST /v1/wallet-accounts \
    "{\"purpose\":\"CAMPAIGN\",\"reference_type\":\"SETTLEMENT_E2E\",\"reference_id\":\"src-$1-$R\",\"label\":\"settlement source $1\"}" "$jwt"
  acct=$(jget id)
  [ -n "$acct" ] || return 1

  # The gross arrives the way it really does: a payer pays a payment session
  # bound to that account. This is the leg the arithmetic in the header depends
  # on — the wallet holds the FULL amount, and the operator's rate is charged
  # one step later, at the settlement.
  call "$GW" 8080 POST /v1/payment-sessions \
    "{\"wallet_account_id\":\"$acct\",\"purpose\":\"DONATION\",\"reference_type\":\"SETTLEMENT_E2E\",\"reference_id\":\"pay-$1-$R\",\"amount_minor\":$GROSS,\"currency\":\"AOA\"}" "$jwt"
  local slug
  slug=$(printf '%s' "$LAST" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{const j=JSON.parse(s);const i=(j.interfaces||[]).find(x=>x.type==="PAYMENT_LINK")||(j.interfaces||[]).find(x=>x.type==="DEEP_LINK");process.stdout.write(i?String(i.value).split("/").filter(Boolean).pop():"")}catch(e){}})')
  [ -n "$slug" ] || return 1
  call "$PUB" 8083 POST "/v1/payment-links/$slug/pay" "{\"amount_minor\":$GROSS}" "$PAYER_JWT"

  printf '%s|%s|%s|%s' "$mid" "$acct" "$ownh" "$BENEFICIARY_HANDLE"
}

settle(){ # $1=merchant $2=source_account $3=beneficiary_@banza $4=idem $5=own_@banza
  local jwt; jwt=$(mint "$1")
  call "$GW" 8080 POST /v1/application-settlements \
    "{\"idempotency_key\":\"settle-$4-$R\",\"source_account_id\":\"$2\",\"beneficiary_banza_name\":\"$3\",\"fee_destination_banza_name\":\"$5\",\"reason\":\"CAMPAIGN_CLOSE\",\"reference_type\":\"SETTLEMENT_E2E\",\"reference_id\":\"ref-$4-$R\"}" "$jwt"
}

run_case(){ # $1 = label
  local label="$1" mid acct ownh benh before
  IFS='|' read -r mid acct ownh benh <<<"$(mkowner "$label")"
  chk "${label}_OWNER_READY" "$([ -n "$mid" ] && [ -n "$acct" ] && [ -n "$ownh" ] && [ -n "$benh" ] && echo yes)" yes
  chk "${label}_PROFILE_ASSIGNED" "$(q "SELECT p.code FROM merchants m JOIN pricing_profiles p ON p.id=m.pricing_profile_id WHERE m.id='$mid'")" "sandbox-reference"
  # The campaign account holds the FULL amount: the payment leg is neutral, and
  # the operator's rate is charged one step later.
  chk "${label}_CAMPAIGN_HOLDS_GROSS" "$(abal "$acct")" "$GROSS"

  before=$(hbal "$benh")
  settle "$mid" "$acct" "$benh" "$label" "$ownh"
  chk "${label}_SETTLEMENT_ACCEPTED" "$CODE" "201"
  local sid; sid=$(jget id)

  local row fee net rate
  fee=$(q "SELECT application_fee_minor FROM app_settlements WHERE id='$sid'")
  net=$(q "SELECT net_amount_minor FROM app_settlements WHERE id='$sid'")
  rate=$(q "SELECT (pricing_snapshot_json->>'rate_bps') FROM app_settlements WHERE id='$sid'")
  chk "${label}_RATE_IS_200BPS" "$rate" "$RATE_BPS"
  chk "${label}_FEE" "$fee" "$EXPECTED_FEE"
  chk "${label}_NET" "$net" "$EXPECTED_NET"
  chk "${label}_RULE_ATTRIBUTED" "$(q "SELECT (pricing_rule_id IS NOT NULL)::text FROM app_settlements WHERE id='$sid'")" "true"
  chk "${label}_BENEFICIARY_GETS_NET" "$(( $(hbal "$benh") - before ))" "$EXPECTED_NET"
  # The campaign account is emptied: net to the beneficiary, fee to the owner's
  # own business account. Nothing is left behind, which is what closing a
  # campaign means.
  chk "${label}_SOURCE_EMPTIED" "$(abal "$acct")" "0"
  # And the fee is where ADR-028 says it must be — the caller's own account,
  # named by the caller's own @banza.
  chk "${label}_FEE_TO_OWN_ACCOUNT" "$(q "SELECT COALESCE(SUM(CASE WHEN e.entry_type='CREDIT' THEN e.amount_minor ELSE -e.amount_minor END),0)
        FROM ledger_entries e WHERE e.posting_id=(SELECT fee_posting_id FROM app_settlements WHERE id='$sid')
          AND e.account_id=(SELECT available_account_id FROM wallets WHERE merchant_id='$mid')")" "$EXPECTED_FEE"
  # Record for the parity comparison.
  printf '%s' "$fee" > "/tmp/fee-$label-$R"
  printf '%s' "$net" > "/tmp/net-$label-$R"
}

echo
echo "### an ordinary controlled owner"
run_case ORDINARY

echo
echo "### a second owner on the SAME generic profile — the reference-application shape"
run_case REFERENCE

echo
echo "### economic parity: the differential must be exactly zero"
FEE_A=$(cat "/tmp/fee-ORDINARY-$R" 2>/dev/null); FEE_B=$(cat "/tmp/fee-REFERENCE-$R" 2>/dev/null)
NET_A=$(cat "/tmp/net-ORDINARY-$R" 2>/dev/null); NET_B=$(cat "/tmp/net-REFERENCE-$R" 2>/dev/null)
chk PARITY_FEE_DIFFERENTIAL "$(( ${FEE_A:-0} - ${FEE_B:-1} ))" "0"
chk PARITY_NET_DIFFERENTIAL "$(( ${NET_A:-0} - ${NET_B:-1} ))" "0"
rm -f "/tmp/fee-ORDINARY-$R" "/tmp/fee-REFERENCE-$R" "/tmp/net-ORDINARY-$R" "/tmp/net-REFERENCE-$R"

echo
echo "### the ledger is exactly as sound as it was"
chk LEDGER_SOUND_AFTER "$(unbalanced)" "0"

echo
echo "SETTLEMENT_ECONOMICS_E2E: PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
