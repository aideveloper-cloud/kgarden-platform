#!/usr/bin/env bash
# End-to-end test of signing in to the WMS (production-pos) with access managed in ERPNext.
#
#   POS tablet: employee code + PIN at a warehouse, checked by ERPNext (app WMS, warehouse scope);
#   the cut/receipt is recorded under that person. Dashboard: "เข้าสู่ระบบด้วย K Garden" (OIDC).
#   Password login only for break-glass accounts. Access removed in ERPNext -> WMS sessions end.
#
# Usage: set -a; . ./.env; set +a; ERPNEXT_ADMIN_PASSWORD=... WMS_URL=http://127.0.0.1:8300 \
#        WMS_APP_DIR=~/Desktop/production-pos/point-of-sales scripts/e2e_wms_auth.sh   (local copies only)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WMS=${WMS_URL:?}
WMS_APP_DIR=${WMS_APP_DIR:?}
ERP=${ERPNEXT_URL:?}
ADMIN_PW=${ERPNEXT_ADMIN_PASSWORD:?}
RUN=$(date +%s)
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
pass=0
check() {
	if [[ "$2" == "$3" ]]; then echo "  PASS  $1 ($2)"; pass=$((pass + 1)); else echo "  FAIL  $1: got '$2', expected '$3'"; exit 1; fi
}
erp() { curl -sS -b "$W/erp_admin" -X "$1" -H 'Content-Type: application/json' "$ERP$2" ${3:+--data "$3"}; }
page() { # jar path -> Inertia page json
	curl -sS -b "$W/$1" -c "$W/$1" -L "$WMS$2" | python3 "$ROOT/scripts/inertia_page.py"
}
xsrf() { python3 -c "import sys,urllib.parse;[print(urllib.parse.unquote(l.split()[-1])) for l in open(sys.argv[1]) if '\tXSRF-TOKEN\t' in l]" "$W/$1" | tail -1; }
post() { # jar path json -> final page json (follows the redirect)
	# --data without -X: curl turns the POST into a GET when it follows the redirect.
	curl -sS -b "$W/$1" -c "$W/$1" -L "$WMS$2" -H 'Content-Type: application/json' -H 'Accept: text/html' \
		-H "X-XSRF-TOKEN: $(xsrf "$1")" --data "$3" | python3 "$ROOT/scripts/inertia_page.py"
}
wh_id() { "$ROOT/scripts/wms_sql.sh" "SELECT id FROM warehouses WHERE code = '$1'"; }

curl -sS -c "$W/erp_admin" -X POST "$ERP/api/method/login" -H 'Content-Type: application/json' \
	--data "$(jq -cn --arg p "$ADMIN_PW" '{usr:"Administrator", pwd:$p}')" >/dev/null
DECHA="โกดังเดชา / ผลิตเสา+ประตู - KG"
person() { # email first code grants-json
	erp POST /api/resource/User "$(jq -cn --arg e "$1" --arg n "$2" --arg c "$3" --argjson g "$4" \
		'{email:$e, first_name:$n, send_welcome_email:0, kg_employee_code:$c, kg_app_access:$g}')" >/dev/null
}
OP="wms.op.$RUN@example.com"; OP_CODE="WOP$RUN"; OP_PIN="$((RANDOM % 900000 + 100000))"
NO="wms.no.$RUN@example.com"; NO_CODE="WNO$RUN"
ADM="wms.adm.$RUN@example.com"; ADM_PW="Wms-$RUN-x9"; OP_PW="Wop-$RUN-x9"
person "$OP" "พนักงานคลังเดชา" "$OP_CODE" "$(jq -cn --arg w "$DECHA" '[{app:"WMS", app_role:"operator", scope_value:$w}]')"
person "$NO" "ไม่มีสิทธิ์ WMS" "$NO_CODE" '[{"app":"MES_FLOOR","app_role":"operator"}]'
person "$ADM" "หัวหน้าคลัง" "WAD$RUN" '[{"app":"WMS","app_role":"admin"}]'
for u in "$OP:$OP_PIN" "$NO:$OP_PIN"; do erp POST /api/method/kgarden.access.api.set_pin "$(jq -cn --arg u "${u%%:*}" --arg p "${u##*:}" '{user:$u, pin:$p}')" >/dev/null; done
erp PUT "/api/resource/User/$OP" "$(jq -cn --arg p "$OP_PW" '{new_password:$p}')" >/dev/null
erp PUT "/api/resource/User/$ADM" "$(jq -cn --arg p "$ADM_PW" '{new_password:$p}')" >/dev/null
DECHA_ID=$(wh_id WH-DECHA-POST); PHRANON_ID=$(wh_id WH-PHRANON)
unlock() { # jar code pin warehouse-id
	page "$1" /pos/rm >/dev/null
	post "$1" /pos/rm/unlock "$(jq -cn --arg c "$2" --arg p "$3" --argjson w "$4" '{employee_code:$c, pin:$p, warehouse_id:$w}')"
}

echo "1. POS tablet: employee code + PIN"
p=$(unlock pos "$OP_CODE" "$OP_PIN" "$DECHA_ID")
check "operator opens the RM POS at own warehouse" "$(jq -r .component <<<"$p")" "Pos/Index"
check "operator is the person, locked" "$(jq -r '"\(.props.operatorLocked) \(.props.operatorName)"' <<<"$p")" "true พนักงานคลังเดชา ($OP_CODE)"
check "wrong PIN refused" "$(unlock bad "$OP_CODE" 000000 "$DECHA_ID" | jq -r .component)" "Pos/Unlock"
check "another warehouse refused" "$(unlock other "$OP_CODE" "$OP_PIN" "$PHRANON_ID" | jq -r .component)" "Pos/Unlock"
check "person without WMS access refused" "$(unlock no "$NO_CODE" "$OP_PIN" "$DECHA_ID" | jq -r .component)" "Pos/Unlock"

echo "2. A receipt is recorded under the signed-in person, whatever name the request carries"
PIPE=$("$ROOT/scripts/wms_sql.sh" "SELECT id FROM products WHERE sku = 'RM-PIPE'")
post pos /pos/rm/receive "$(jq -cn --argjson w "$DECHA_ID" --argjson p "$PIPE" '{warehouse_id:$w, product_id:$p, qty:1, uom:"PCS", operator_name:"ชื่อปลอม"}')" >/dev/null
who=$("$ROOT/scripts/wms_sql.sh" "SELECT operator_name FROM stock_ledgers ORDER BY id DESC LIMIT 1")
check "ledger operator" "$who" "พนักงานคลังเดชา ($OP_CODE)"

echo "3. Password login is for break-glass accounts only"
# The login form carries a bot-guard token and refuses a submit faster than a person types.
guard=$(page pw /login | jq -c '.props.botGuard')
sleep 3
p=$(post pw /login "$(jq -cn --argjson g "$guard" '{email:"admin@wms.local", password:"anything"} + {($g.token_field): $g.token, ($g.honeypot_field): ""}')")
check "admin@wms.local told to use K Garden sign-in" "$(jq -r '.props.errors.email' <<<"$p" | grep -c 'K Garden')" 1

echo "4. Dashboard: \"เข้าสู่ระบบด้วย K Garden\" through ERPNext"
sso() { # wms-jar erp-email erp-password -> http status of the final WMS page
	curl -sS -c "$W/erp_$1" -X POST "$ERP/api/method/login" -H 'Content-Type: application/json' \
		--data "$(jq -cn --arg u "$2" --arg p "$3" '{usr:$u, pwd:$p}')" >/dev/null
	local auth back
	auth=$(curl -sS -b "$W/$1" -c "$W/$1" -o /dev/null -w '%{redirect_url}' "$WMS/auth/kgarden/redirect")
	back=$(curl -sS -b "$W/erp_$1" -o /dev/null -w '%{http_code} %{redirect_url}' "$auth")
	[[ ${back%% *} == 302 ]] || { echo "${back%% *}"; return; }
	curl -sS -b "$W/$1" -c "$W/$1" -o /dev/null -w '%{redirect_url}' "${back#* }"
}
check "warehouse admin signs in and lands on the dashboard" "$(sso adm "$ADM" "$ADM_PW")" "$WMS/dashboard"
check "dashboard opens" "$(page adm /dashboard | jq -r .component | grep -c Dashboard)" 1
check "operator signs in and lands on the POS" "$(sso op "$OP" "$OP_PW")" "$WMS/pos"
check "a person without WMS access is refused by ERPNext" "$(erp PUT "/api/resource/User/$NO" "$(jq -cn --arg p "$OP_PW" '{new_password:$p}')" >/dev/null; sso no "$NO" "$OP_PW")" 403

echo "5. WMS access removed in ERPNext: the WMS ends the person's sessions"
erp PUT "/api/resource/User/$OP" '{"kg_app_access":[]}' >/dev/null
(cd "$WMS_APP_DIR" && php artisan kgarden:sync-access) | sed 's/^/        /'
check "POS tablet back to the unlock screen" "$(page pos /pos/rm | jq -r .component)" "Pos/Unlock"
check "PIN no longer opens the POS" "$(unlock again "$OP_CODE" "$OP_PIN" "$DECHA_ID" | jq -r .component)" "Pos/Unlock"
roles=$(cd "$WMS_APP_DIR" && php artisan tinker --execute "echo 'ROLES='.App\\Models\\User::where('email', '$OP')->first()?->getRoleNames()->count();" | sed -n 's/^ROLES=//p')
check "no WMS role left" "$roles" 0

echo "ALL $pass CHECKS PASSED"
