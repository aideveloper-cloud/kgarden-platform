#!/usr/bin/env bash
# End-to-end test of signing in to ConstructionERP with access managed in ERPNext (the core).
#
#   "เข้าสู่ระบบด้วย K Garden" (OIDC against ERPNext): role from ERPNext, only the granted projects,
#   refused without CERP access; a returning user signs in again; a changed role or removed access
#   in ERPNext ends the ConstructionERP sessions (KGARDEN_ACCESS_POLL_SECONDS).
#
# Usage: set -a; . ./.env; set +a; ERPNEXT_ADMIN_PASSWORD=... CERP_URL=http://127.0.0.1:8200 scripts/e2e_cerp_auth.sh
set -euo pipefail

CERP=${CERP_URL:?}
ERP=${ERPNEXT_URL:?}
ADMIN_PW=${ERPNEXT_ADMIN_PASSWORD:?}
REDIRECT=http://127.0.0.1:5173/auth/oidc/callback
RUN=$(date +%s)
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
pass=0
check() {
	if [[ "$2" == "$3" ]]; then echo "  PASS  $1 ($2)"; pass=$((pass + 1)); else echo "  FAIL  $1: got '$2', expected '$3'"; exit 1; fi
}
erp() { curl -sS -b "$W/admin" -X "$1" -H 'Content-Type: application/json' "$ERP$2" ${3:+--data "$3"}; }
curl -sS -c "$W/admin" -X POST "$ERP/api/method/login" -H 'Content-Type: application/json' \
	--data "$(jq -cn --arg p "$ADMIN_PW" '{usr:"Administrator", pwd:$p}')" >/dev/null

sso() { # email password -> access token, or "refused <status>"
	curl -sS -c "$W/erp_$1" -X POST "$ERP/api/method/login" -H 'Content-Type: application/json' \
		--data "$(jq -cn --arg u "$1" --arg p "$2" '{usr:$u, pwd:$p}')" >/dev/null
	local cfg url back code
	cfg=$(curl -sS "$CERP/api/v1/users/auth/oidc/config/")
	url="$(jq -r .authorization_endpoint <<<"$cfg")?$(jq -rn --arg c "$(jq -r .client_id <<<"$cfg")" --arg s "$(jq -r .scopes <<<"$cfg")" \
		--arg r "$REDIRECT" '{client_id:$c, response_type:"code", scope:$s, redirect_uri:$r, state:"e2e"} | to_entries | map("\(.key)=\(.value|@uri)") | join("&")')"
	back=$(curl -sS -b "$W/erp_$1" -o /dev/null -w '%{http_code} %{redirect_url}' "$url")
	[[ ${back%% *} == 302 ]] || { echo "refused ${back%% *}"; return; }
	code=$(sed -E 's/.*[?&]code=([^&]+).*/\1/' <<<"${back#* }")
	curl -sS -X POST "$CERP/api/v1/users/auth/oidc/callback/" -H 'Content-Type: application/json' \
		--data "$(jq -cn --arg c "$code" --arg r "$REDIRECT" '{code:$c, redirect_uri:$r}')" | jq -r '.access_token // ("refused " + (.detail|tostring))'
}
api() { curl -sS -o "$W/body" -w '%{http_code}' -H "Authorization: Bearer $1" "$CERP/api/v1$2"; }
role_of() { api "$1" /users/me/ >/dev/null; jq -r .role "$W/body"; }

# Two ConstructionERP projects, as ERPNext knows them (created by cerp-bridge).
projects=$(erp GET "/api/resource/Project?fields=%5B%22name%22,%22kg_cerp_id%22%5D&filters=$(jq -rn '[["kg_cerp_id","is","set"]]|tojson|@uri')&limit_page_length=2&order_by=creation%20asc")
P1=$(jq -r '.data[0].name' <<<"$projects"); P1_ID=$(jq -r '.data[0].kg_cerp_id' <<<"$projects")
P2=$(jq -r '.data[1].name' <<<"$projects"); P2_ID=$(jq -r '.data[1].kg_cerp_id' <<<"$projects")
U="cerp.buyer.$RUN@example.com"; PW="Cerp-$RUN-x9"; NO="cerp.no.$RUN@example.com"
grant() { erp PUT "/api/resource/User/$U" "$(jq -cn --argjson rows "$1" '{kg_app_access:$rows}')" >/dev/null; }
erp POST /api/resource/User "$(jq -cn --arg e "$U" --arg p "$P1" '{email:$e, first_name:"จัดซื้อ ทดสอบ", send_welcome_email:0,
	kg_app_access:[{app:"CERP", app_role:"purchaser", scope_value:$p}]}')" >/dev/null
erp PUT "/api/resource/User/$U" "$(jq -cn --arg p "$PW" '{new_password:$p}')" >/dev/null
erp POST /api/resource/User "$(jq -cn --arg e "$NO" '{email:$e, first_name:"ไม่มีสิทธิ์ CERP", send_welcome_email:0,
	kg_app_access:[{app:"MES_FLOOR", app_role:"operator"}]}')" >/dev/null
erp PUT "/api/resource/User/$NO" "$(jq -cn --arg p "$PW" '{new_password:$p}')" >/dev/null

echo "1. Purchaser signs in through ERPNext"
T=$(sso "$U" "$PW")
check "signed in" "$([[ $T == refused* ]] && echo "$T" || echo ok)" ok
check "role from ERPNext (purchaser -> editor)" "$(role_of "$T")" editor
api "$T" /projects/ >/dev/null
check "sees only the granted project" "$(jq -r '[ (if type == "array" then . else .items end)[] | .id ] | sort | join(",")' "$W/body")" "$P1_ID"

echo "2. Signing in again works (a returning OIDC user was refused before)"
T=$(sso "$U" "$PW")
check "second sign-in" "$([[ $T == refused* ]] && echo "$T" || echo ok)" ok

echo "3. Someone without ConstructionERP access is refused by ERPNext"
check "refused at ERPNext" "$(sso "$NO" "$PW")" "refused 403"

echo "4. Role and projects changed in ERPNext: old session ends, new sign-in has the new ones"
grant "$(jq -cn --arg p "$P2" '[{app:"CERP", app_role:"approver", scope_value:$p}]')"
for _ in $(seq 1 30); do [[ $(api "$T" /users/me/) == 401 ]] && break; sleep 1; done
check "old token refused" "$(api "$T" /users/me/)" 401
T=$(sso "$U" "$PW")
check "new role (approver -> manager)" "$(role_of "$T")" manager
api "$T" /projects/ >/dev/null
check "now sees only the newly granted project" "$(jq -r '[ (if type == "array" then . else .items end)[] | .id ] | sort | join(",")' "$W/body")" "$P2_ID"

echo "5. ConstructionERP access removed in ERPNext"
grant '[]'
for _ in $(seq 1 30); do [[ $(api "$T" /users/me/) == 401 ]] && break; sleep 1; done
check "session ended" "$(api "$T" /users/me/)" 401
check "cannot sign in again" "$(sso "$U" "$PW")" "refused 403"

echo "6. Password login is for break-glass accounts only"
r=$(curl -sS -o "$W/pw" -w '%{http_code}' -X POST "$CERP/api/v1/users/auth/login/" -H 'Content-Type: application/json' \
	--data "$(jq -cn --arg e "$U" '{email:$e, password:"wrong-or-right"}')")
check "K Garden account cannot use a ConstructionERP password" "$r" 401

echo "ALL $pass CHECKS PASSED"
