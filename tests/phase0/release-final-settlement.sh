#!/usr/bin/env bash
# The release's closing settlement, with the parties the release names.
#
# @doa closes a campaign of 100 000 Kz: the operator's assigned rate takes
# 2 000 to @doa's own business account, and 98 000 reaches @fm65.
#
# A FRESH campaign account, funded by a fresh payer. No existing campaign is
# touched: the Sandbox holds real donor-funded balances, and proving the
# arithmetic does not require moving somebody's donation.
#
# It is a settlement between two named, real parties rather than two fixtures,
# which is what makes it different from settlement-economics-e2e.sh. That file
# proves the model — two owners on one profile charged identically. This proves
# the model applied to the accounts the platform actually runs, including that
# @doa carries the ADR-028 taxonomy and KYB it needs to be its own fee
# destination. Both had to be repaired to get here: all nine self-service
# Businesses were MERCHANT, and a caller-supplied rate used to override pricing.
#
# Re-runnable. Each run creates its own campaign and donor and leaves the
# ledger balanced; nothing is reused between runs.
set -uo pipefail
R="${RANDOM}${RANDOM}"
GROSS=100000
GW=$(docker ps  --format '{{.Names}}' | grep api-gateway-staging | head -1)
PUB=$(docker ps --format '{{.Names}}' | grep public-api-staging  | head -1)
PG=$(docker ps  --format '{{.Names}}' | grep postgres | grep bzsandbox | head -1)
CORE=$(docker ps --format '{{.Names}}'| grep core-api-staging    | head -1)
PW=$(docker exec "$CORE" sh -c 'cat /run/secrets/db_url' | sed -E 's#.*://[^:]+:([^@]+)@.*#\1#')
JWTSEC=$(docker exec "$GW" sh -c 'cat /run/secrets/jwt_secret')
q(){ docker exec -e PGPASSWORD="$PW" "$PG" psql -q -U bl_app_runtime -d banzami_staging -At -c "$1" 2>/dev/null | tr -d '\r\n'; }
LAST=""; CODE=""
call(){ local ct="$1" port="$2" m="$3" p="$4" bd="$5" au="${6:--}"
  local a=(curl -s -w $'\n%{http_code}' -X "$m" "http://localhost:$port$p")
  [ "$au" != "-" ] && a+=(-H "Authorization: Bearer $au")
  local r
  if [ "$bd" = "-" ]; then r=$(docker exec "$ct" "${a[@]}" 2>/dev/null)
  else a+=(-H "Content-Type: application/json" --data @-); r=$(printf '%s' "$bd" | docker exec -i "$ct" "${a[@]}" 2>/dev/null); fi
  CODE=$(printf '%s' "$r" | tail -n1); LAST=$(printf '%s' "$r" | sed '$d'); }
jget(){ printf '%s' "$LAST" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{process.stdout.write(String(JSON.parse(s)["'"$1"'"]??""))}catch(e){}})'; }
mint(){ SECRET="$JWTSEC" K="$1" V="$2" node -e 'const c=require("crypto");const b=o=>Buffer.from(typeof o==="string"?o:JSON.stringify(o)).toString("base64url");const n=Math.floor(Date.now()/1000);const cl={scopes:["*"],environment:"SANDBOX",iat:n,exp:n+900};cl[process.env.K]=process.env.V;const h=b({alg:"HS256",typ:"JWT"}),p=b(cl);process.stdout.write(h+"."+p+"."+c.createHmac("sha256",process.env.SECRET).update(h+"."+p).digest("base64url"));'; }
hbal(){ q "SELECT COALESCE(SUM(CASE WHEN e.entry_type='CREDIT' THEN e.amount_minor ELSE -e.amount_minor END),0)
             FROM ledger_entries e WHERE e.account_id=(SELECT w.available_account_id FROM consumer_wallets w
               JOIN consumers c ON c.id=w.consumer_id WHERE c.handle='$1' AND w.status='ACTIVE' LIMIT 1)"; }
mbal(){ q "SELECT COALESCE(SUM(CASE WHEN e.entry_type='CREDIT' THEN e.amount_minor ELSE -e.amount_minor END),0)
             FROM ledger_entries e WHERE e.account_id=(SELECT available_account_id FROM wallets WHERE merchant_id='$1')"; }
abal(){ q "SELECT COALESCE(SUM(CASE WHEN e.entry_type='CREDIT' THEN e.amount_minor ELSE -e.amount_minor END),0)
             FROM ledger_entries e WHERE e.account_id=(SELECT account_id FROM wallet_accounts WHERE id='$1')"; }
unbalanced(){ q "SELECT COUNT(*) FROM (SELECT p.id FROM ledger_postings p JOIN ledger_entries e ON e.posting_id=p.id
                  GROUP BY p.id HAVING SUM(CASE e.entry_type WHEN 'DEBIT' THEN -e.amount_minor ELSE e.amount_minor END) <> 0) x"; }
P=0; F=0
chk(){ if [ "$2" = "$3" ]; then echo "  $1 PASS ($2)"; P=$((P+1)); else echo "  $1 FAIL (got '$2' want '$3')"; F=$((F+1)); fi; }

DOA=$(q "SELECT owner_id FROM handle_registry WHERE handle='doa' AND owner_type='MERCHANT'")
DOAW=$(q "SELECT id FROM wallets WHERE merchant_id='$DOA' AND currency='AOA' AND status='ACTIVE' LIMIT 1")
echo "### @doa closes a 100 000 Kz campaign; @fm65 is the beneficiary"
chk DOA_RESOLVED "$([ -n "$DOA" ] && [ -n "$DOAW" ] && echo yes)" yes
chk DOA_IS_APPLICATION "$(q "SELECT business_account_type FROM merchants WHERE id='$DOA'")" APPLICATION
chk DOA_KYB_APPROVED "$(q "SELECT kyb_status FROM merchant_compliance WHERE merchant_id='$DOA'")" APPROVED
chk DOA_PROFILE "$(q "SELECT p.code FROM merchants m JOIN pricing_profiles p ON p.id=m.pricing_profile_id WHERE m.id='$DOA'")" sandbox-reference
chk FM65_EXISTS "$(q "SELECT count(*) FROM consumers WHERE handle='fm65'")" 1
chk LEDGER_SOUND_BEFORE "$(unbalanced)" 0

DOAJWT=$(mint merchant_id "$DOA")
call "$GW" 8080 POST /v1/wallet-accounts \
  "{\"wallet_id\":\"$DOAW\",\"purpose\":\"CAMPAIGN\",\"reference_type\":\"RELEASE_A_FINAL\",\"reference_id\":\"final-$R\",\"label\":\"Release A final settlement $R\"}" "$DOAJWT"
ACCT=$(jget id)
chk CAMPAIGN_CREATED "$([ -n "$ACCT" ] && echo yes)" yes
[ -n "$ACCT" ] || { echo "cannot continue"; exit 1; }

# A fresh donor pays the campaign, so the gross arrives through the payment leg.
PH="+2449${R:0:4}90"; H="fin${R:0:5}"
call "$PUB" 8083 POST /v1/consumer/onboarding/start "{\"phone_number\":\"$PH\",\"currency\":\"AOA\",\"otp_plaintext_for_test\":\"123456\"}" -
SID=$(jget session_id)
call "$PUB" 8083 POST /v1/consumer/onboarding/verify-otp "{\"session_id\":\"$SID\",\"otp_code\":\"123456\"}" -
call "$PUB" 8083 POST /v1/consumer/onboarding/complete "{\"session_id\":\"$SID\",\"banza_handle\":\"$H\",\"pin\":\"1234\"}" -
DONOR=$(jget consumer_id)
DJWT=$(mint customer_id "$DONOR")
call "$GW" 8080 POST /v1/compliance/customers/verify \
  "{\"full_name\":\"RELEASE A DONOR\",\"document_type\":\"BILHETE_DE_IDENTIDADE\",\"document_number\":\"RA$R\",\"date_of_birth\":\"1990-01-01\",\"requested_level\":\"BASIC\"}" "$DJWT"
call "$PUB" 8083 POST /v1/sandbox/fund "{\"amount_minor\":$((GROSS*2)),\"currency\":\"AOA\"}" "$DJWT"
chk DONOR_FUNDED "$([ "$(hbal "$H")" -ge "$GROSS" ] && echo yes)" yes

call "$GW" 8080 POST /v1/payment-sessions \
  "{\"wallet_account_id\":\"$ACCT\",\"purpose\":\"DONATION\",\"reference_type\":\"RELEASE_A_FINAL\",\"reference_id\":\"don-$R\",\"amount_minor\":$GROSS,\"currency\":\"AOA\"}" "$DOAJWT"
SLUG=$(printf '%s' "$LAST" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{const j=JSON.parse(s);const i=(j.interfaces||[]).find(x=>x.type==="PAYMENT_LINK");process.stdout.write(i?String(i.value).split("/").filter(Boolean).pop():"")}catch(e){}})')
call "$PUB" 8083 POST "/v1/payment-links/$SLUG/pay" "{\"amount_minor\":$GROSS}" "$DJWT"
chk CAMPAIGN_HOLDS_GROSS "$(abal "$ACCT")" "$GROSS"

DOA_BEFORE=$(mbal "$DOA"); FM65_BEFORE=$(hbal fm65)
echo "  @doa business account before: $DOA_BEFORE · @fm65 before: $FM65_BEFORE"

call "$GW" 8080 POST /v1/application-settlements \
  "{\"idempotency_key\":\"release-a-final-$R\",\"source_account_id\":\"$ACCT\",\"beneficiary_banza_name\":\"fm65\",\"fee_destination_banza_name\":\"doa\",\"reason\":\"CAMPAIGN_CLOSE\",\"reference_type\":\"RELEASE_A_FINAL\",\"reference_id\":\"final-$R\"}" "$DOAJWT"
chk SETTLEMENT_ACCEPTED "$CODE" 201
SID2=$(jget id)
chk FEE_IS_2000  "$(q "SELECT application_fee_minor FROM app_settlements WHERE id='$SID2'")" 2000
chk NET_IS_98000 "$(q "SELECT net_amount_minor FROM app_settlements WHERE id='$SID2'")" 98000
chk RATE_IS_200BPS "$(q "SELECT pricing_snapshot_json->>'rate_bps' FROM app_settlements WHERE id='$SID2'")" 200
chk RULE_ATTRIBUTED "$(q "SELECT rule_key FROM pricing_rules WHERE id=(SELECT pricing_rule_id FROM app_settlements WHERE id='$SID2')")" sandbox-reference-settlement
chk FM65_RECEIVES_NET "$(( $(hbal fm65) - FM65_BEFORE ))" 98000
chk DOA_RECEIVES_FEE  "$(( $(mbal "$DOA") - DOA_BEFORE ))" 2000
chk CAMPAIGN_EMPTIED "$(abal "$ACCT")" 0
chk LEDGER_SOUND_AFTER "$(unbalanced)" 0
echo
echo "RELEASE_A_FINAL_SETTLEMENT: PASS=$P FAIL=$F"
[ "$F" -eq 0 ] || exit 1
