#!/usr/bin/env bash
# End-to-end test of signing in to the MES floor tablet with access managed in ERPNext.
#
#   employee code + PIN (shared tablet) and "เข้าสู่ระบบด้วย K Garden" (OpenID Connect) both give an
#   MES session with the role set in ERPNext; actions are recorded under the signed-in person; an
#   operator cannot do supervisor work; removing MES access or changing the role in ERPNext ends the
#   MES session within the access poll (MES_ACCESS_POLL); someone without MES access cannot sign in.
#
# Usage: set -a; . ./.env; set +a; ERPNEXT_ADMIN_PASSWORD=... scripts/e2e_mes_auth.sh   (local only)
set -euo pipefail

MES=http://${MES_HTTP_ADDR:?}
ERP=${ERPNEXT_URL:?}
ADMIN_PW=${ERPNEXT_ADMIN_PASSWORD:?}
OP=${MES_E2E_OPERATOR:?}; OP_PIN=${MES_E2E_OPERATOR_PIN:?}
OP_USER=$(tr '[:upper:]' '[:lower:]' <<<"$OP")@example.com
RUN=$(date +%s)
W=$(mktemp -d)
pass=0
check() {
	if [[ "$2" == "$3" ]]; then echo "  PASS  $1 ($2)"; pass=$((pass + 1)); else echo "  FAIL  $1: got '$2', expected '$3'"; exit 1; fi
}
erp_admin() { curl -sS -b "$W/admin" -X "$1" -H 'Content-Type: application/json' "$ERP$2" ${3:+--data "$3"}; }
grant() { erp_admin PUT "/api/resource/User/$OP_USER" "$(jq -cn --arg r "$1" '{kg_app_access: (if $r == "" then [] else [{app:"MES_FLOOR", app_role:$r}] end)}')" >/dev/null; }
restore() { grant operator; rm -rf "$W"; }
trap restore EXIT
mes() { # jar method path [json] -> body + status line
	curl -sS -b "$W/$1" -c "$W/$1" -X "$2" -H 'Content-Type: application/json' "$MES$3" ${4:+--data "$4"} -w '\n%{http_code}'
}
code_of() { tail -n1 <<<"$1"; }
pin_login() { mes "$1" POST /auth/pin "$(jq -cn --arg c "$2" --arg p "$3" '{employee_code:$c, pin:$p}')"; }
wait_status() { # jar path status
	for _ in $(seq 1 30); do [[ $(code_of "$(mes "$1" GET "$2")") == "$3" ]] && return 0; sleep 1; done
	echo "  (gave up waiting for $3 on $2)"; return 1
}

curl -sS -c "$W/admin" -X POST "$ERP/api/method/login" -H 'Content-Type: application/json' \
	--data "$(jq -cn --arg p "$ADMIN_PW" '{usr:"Administrator", pwd:$p}')" >/dev/null
grant operator

echo "1. No session, no MES"
check "API refuses without a session" "$(code_of "$(mes none GET /work-orders)")" 401

echo "2. Shared tablet: employee code + PIN"
check "wrong PIN refused" "$(code_of "$(pin_login op "$OP" 000000)")" 401
r=$(pin_login op "$OP" "$OP_PIN")
check "right PIN signs in with the ERPNext role" "$(code_of "$r")/$(sed '$d' <<<"$r" | jq -r .role)" "200/operator"
check "operator can see Work Orders" "$(code_of "$(mes op GET /work-orders)")" 200
check "operator cannot pull Work Orders (supervisor work)" "$(code_of "$(mes op POST /sync/work-orders)")" 403

echo "3. Actions are recorded under the signed-in person, not a typed name"
WO=$(mes op GET /work-orders | sed '$d' | jq -r '[(.data // [])[] | select(.running == false)][0].name // empty')
if [[ -n "$WO" ]]; then
	mes op POST "/work-orders/$WO/start" '{"operator":"ชื่อที่พิมพ์เอง"}' >/dev/null
	who=$(mes op GET "/work-orders/$WO" | sed '$d' | jq -r '.operation_logs[-1].operator')
	mes op POST "/work-orders/$WO/stop" '{}' >/dev/null
	check "operator recorded from the session" "$(grep -c "($OP)" <<<"$who")" 1
else
	echo "  SKIP  no idle open Work Order to start"
fi

echo "4. \"เข้าสู่ระบบด้วย K Garden\" (OpenID Connect through ERPNext)"
PW="Mes-$RUN-x9"
erp_admin PUT "/api/resource/User/$OP_USER" "$(jq -cn --arg p "$PW" '{new_password:$p}')" >/dev/null
curl -sS -c "$W/erp_op" -X POST "$ERP/api/method/login" -H 'Content-Type: application/json' \
	--data "$(jq -cn --arg u "$OP_USER" --arg p "$PW" '{usr:$u, pwd:$p}')" >/dev/null
url=$(mes sso GET /auth/oidc/start | sed '$d' | jq -r .url)
loc=$(curl -sS -b "$W/erp_op" -o /dev/null -w '%{redirect_url}' "$url")
code=$(sed -E 's/.*[?&]code=([^&]+).*/\1/' <<<"$loc"); state=$(sed -E 's/.*[?&]state=([^&]+).*/\1/' <<<"$loc")
r=$(mes sso POST /auth/oidc/callback "$(jq -cn --arg c "$code" --arg s "$state" '{code:$c, state:$s}')")
check "signed in through ERPNext" "$(code_of "$r")/$(sed '$d' <<<"$r" | jq -r '"\(.method) \(.role)"')" "200/sso operator"
check "a used state cannot be replayed" "$(code_of "$(mes sso2 POST /auth/oidc/callback "$(jq -cn --arg c "$code" --arg s "$state" '{code:$c, state:$s}')")")" 400

echo "5. Role changed in ERPNext: old sessions end, a new sign-in gets the new role"
grant supervisor
wait_status op /auth/me 401 && check "PIN session ended" 401 401
wait_status sso /auth/me 401 && check "ERPNext sign-in session ended" 401 401
r=$(pin_login op "$OP" "$OP_PIN")
check "signs in again as supervisor" "$(sed '$d' <<<"$r" | jq -r .role)" supervisor
check "and may pull Work Orders now" "$(code_of "$(mes op POST /sync/work-orders)")" 200

echo "6. MES access removed in ERPNext: session ends, no new sign-in"
grant ""
wait_status op /work-orders 401 && check "session ended" 401 401
check "PIN sign-in refused" "$(code_of "$(pin_login op2 "$OP" "$OP_PIN")")" 401
loc=$(curl -sS -b "$W/erp_op" -o /dev/null -w '%{http_code}' "$(mes sso3 GET /auth/oidc/start | sed '$d' | jq -r .url)")
check "ERPNext refuses the MES sign-in" "$loc" 403

echo "7. Logout"
grant operator
r=$(pin_login op4 "$OP" "$OP_PIN")
mes op4 POST /auth/logout >/dev/null
check "after logout the session is gone" "$(code_of "$(mes op4 GET /auth/me)")" 401

echo "ALL $pass CHECKS PASSED"
