#!/usr/bin/env bash
# End-to-end test of app access managed in ERPNext (the core), through the real sign-in flow.
#
# One person gets access only to the apps ticked on their User form: ERPNext signs them in to those
# (OpenID Connect) and refuses the rest; a super admin opens every app; only a super admin may change
# access; taking access away revokes the app's tokens at once; shared tablets identify people by
# employee code + PIN through the access API.
#
# Usage: set -a; . ./.env; set +a; ERPNEXT_ADMIN_PASSWORD=... scripts/e2e_access.sh   (local ERPNext only)
set -euo pipefail

ERP=${ERPNEXT_URL:?}
ADMIN_PW=${ERPNEXT_ADMIN_PASSWORD:?Administrator password of the local site}
RUN=$(date +%s)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
pass=0

check() {
	if [[ "$2" == "$3" ]]; then echo "  PASS  $1 ($2)"; pass=$((pass + 1)); else echo "  FAIL  $1: got '$2', expected '$3'"; exit 1; fi
}
login() { # jar user password
	curl -sS -c "$WORK/$1" -X POST "$ERP/api/method/login" -H 'Content-Type: application/json' \
		--data "$(jq -cn --arg u "$2" --arg p "$3" '{usr:$u, pwd:$p}')" | jq -r '.message'
}
as() { # jar method path [json]
	curl -sS -b "$WORK/$1" -X "$2" -H 'Content-Type: application/json' -H 'Accept: application/json' "$ERP$3" ${4:+--data "$4"}
}
client() { as admin GET "/api/resource/KG%20App/$1" | jq -r .data.oauth_client; }
secret() { as admin GET "/api/method/frappe.client.get_password?doctype=OAuth%20Client&name=$1&fieldname=client_secret" | jq -r .message; }
redirect_of() { as admin GET "/api/resource/OAuth%20Client/$1" | jq -r .data.default_redirect_uri; }
authorize() { # jar client -> prints "<http status> <code or ->"
	local r loc
	r=$(curl -sS -b "$WORK/$1" -o "$WORK/page" -w '%{http_code} %{redirect_url}' -G "$ERP/api/method/frappe.integrations.oauth2.authorize" \
		--data-urlencode "client_id=$2" --data-urlencode "response_type=code" --data-urlencode "scope=openid all" \
		--data-urlencode "redirect_uri=$(redirect_of "$2")" --data-urlencode "state=e2e")
	loc=${r#* }
	if [[ "$loc" == *"code="* ]]; then echo "${r%% *} $(sed -E 's/.*[?&]code=([^&]+).*/\1/' <<<"$loc")"; else echo "${r%% *} -"; fi
}
token() { # client code -> access token (in the header or the body, as the client is set up)
	local sec auth; sec=$(secret "$1")
	if [[ $(as admin GET "/api/resource/OAuth%20Client/$1" | jq -r .data.token_endpoint_auth_method) == "Client Secret Post" ]]; then
		auth=(--data-urlencode "client_secret=$sec")
	else
		auth=(-u "$1:$sec")
	fi
	curl -sS "${auth[@]}" -X POST "$ERP/api/method/frappe.integrations.oauth2.get_token" \
		--data-urlencode grant_type=authorization_code --data-urlencode "code=$2" \
		--data-urlencode "redirect_uri=$(redirect_of "$1")" --data-urlencode "client_id=$1" | jq -r .access_token
}
userinfo() { curl -sS -H "Authorization: Bearer $1" "$ERP/api/method/frappe.integrations.oauth2.openid_profile"; }
new_user() { # email first-name extra-json
	as admin POST /api/resource/User "$(jq -cn --arg e "$1" --arg n "$2" --argjson x "${3:-{\}}" '{email:$e, first_name:$n, send_welcome_email:0} + $x')" >/dev/null
	as admin POST /api/method/frappe.core.doctype.user.user.update_password "$(jq -cn --arg e "$1" --arg p "Kg!$RUN-$RANDOM" '{new_password:$p, logout_all_sessions:0, key:"", old_password:""}')" >/dev/null || true
}
set_password() { # user password (as admin)
	as admin PUT "/api/resource/User/$1" "$(jq -cn --arg p "$2" '{new_password:$p}')" >/dev/null
}

[[ $(login admin Administrator "$ADMIN_PW") == "Logged In" ]] || { echo "admin login failed"; exit 1; }
WMS=$(client WMS); CERP=$(client CERP)
WH="โกดังเดชา / ผลิตเสา+ประตู - KG"
A="e2e.wms.$RUN@example.com"; B="e2e.super.$RUN@example.com"; C="e2e.sysman.$RUN@example.com"; S="e2e.service.$RUN@example.com"
PW_A="Kg-a-$RUN-x9"; PW_B="Kg-b-$RUN-x9"; PW_C="Kg-c-$RUN-x9"

echo "1. A super admin creates a warehouse operator with access to the WMS only"
as admin POST /api/resource/User "$(jq -cn --arg e "$A" --arg c "E2E$RUN" --arg wh "$WH" '{email:$e, first_name:"พนักงานคลัง ทดสอบ",
	send_welcome_email:0, kg_employee_code:$c, kg_app_access:[{app:"WMS", app_role:"operator", scope_value:$wh}]}')" | jq -r '.data.name // .exc_type' >/dev/null
set_password "$A" "$PW_A"
as admin POST /api/method/kgarden.access.api.set_pin "$(jq -cn --arg u "$A" '{user:$u, pin:"4826"}')" >/dev/null
u=$(as admin GET "/api/resource/User/$A")
check "has the WMS access role" "$(jq -r '[.data.roles[].role] | index("KG App: WMS") != null' <<<"$u")" true
check "no ERPNext desk (website user)" "$(jq -r .data.user_type <<<"$u")" "Website User"

echo "2. Signing in to the WMS through ERPNext works and carries role + warehouse"
[[ $(login a "$A" "$PW_A") != "" ]]
read -r st code <<<"$(authorize a "$WMS")"
check "authorize WMS redirects with a code" "$st/$([[ $code != - ]] && echo code)" "302/code"
TOK_A=$(token "$WMS" "$code")
info=$(userinfo "$TOK_A")
check "userinfo: app" "$(jq -r .kg_app <<<"$info")" WMS
check "userinfo: role" "$(jq -r .kg_role <<<"$info")" operator
check "userinfo: warehouse scope" "$(jq -r '.kg_scopes[0]' <<<"$info")" "$WH"

echo "3. The same person cannot sign in to ConstructionERP"
read -r st code <<<"$(authorize a "$CERP")"
check "authorize ConstructionERP refused" "$st/$code" "403/-"
check "with the Thai no-access page" "$(grep -q 'ไม่มีสิทธิ์เข้าใช้' "$WORK/page" && echo shown)" shown

echo "4. Only a super admin may change access"
new_user "$C" "System Manager ทดสอบ" '{"roles":[{"role":"System Manager"}]}'
set_password "$C" "$PW_C"
[[ $(login c "$C" "$PW_C") != "" ]]
r=$(as c PUT "/api/resource/User/$A" "$(jq -cn '{kg_app_access:[{app:"WMS", app_role:"operator"},{app:"CERP", app_role:"admin"}]}')")
check "system manager without KG super admin is refused" "$(jq -r .exc_type <<<"$r")" PermissionError
check "A still has no ConstructionERP" "$(as admin GET "/api/resource/User/$A" | jq '[.data.kg_app_access[].app] | index("CERP") == null')" true

echo "5. Floor tablet: employee code + PIN through the access API (service account)"
new_user "$S" "WMS service" '{"roles":[{"role":"KG Access Service"}]}'
keys=$(as admin POST /api/method/frappe.core.doctype.user.user.generate_keys "$(jq -cn --arg u "$S" '{user:$u}')")
SVC="Authorization: token $(as admin GET "/api/resource/User/$S" | jq -r .data.api_key):$(jq -r .message.api_secret <<<"$keys")"
pin() { curl -sS -H "$SVC" -H 'Content-Type: application/json' -X POST "$ERP/api/method/kgarden.access.api.verify_pin" --data "$1" | jq -c .message; }
r=$(pin "$(jq -cn --arg c "E2E$RUN" --arg wh "$WH" '{app_code:"WMS", employee_code:$c, pin:"4826", scope_value:$wh}')")
check "right PIN, own warehouse: allowed as operator" "$(jq -r '"\(.ok)/\(.role)"' <<<"$r")" "true/operator"
check "wrong PIN refused" "$(pin "$(jq -cn --arg c "E2E$RUN" '{app_code:"WMS", employee_code:$c, pin:"0000"}')" | jq -r .ok)" false
check "other warehouse refused" "$(pin "$(jq -cn --arg c "E2E$RUN" '{app_code:"WMS", employee_code:$c, pin:"4826", scope_value:"โกดังพระนอน - KG"}')" | jq -r .ok)" false
check "app not granted refused" "$(pin "$(jq -cn --arg c "E2E$RUN" '{app_code:"MES_FLOOR", employee_code:$c, pin:"4826"}')" | jq -r .ok)" false

echo "6. Taking the WMS away ends access at once"
since=$(TZ=Asia/Bangkok date '+%Y-%m-%d %H:%M:%S')
as admin PUT "/api/resource/User/$A" '{"kg_app_access":[]}' >/dev/null
check "existing WMS token no longer works" "$(userinfo "$TOK_A" | jq -r 'if .email then "still works" else "refused" end')" refused
check "token revoked in ERPNext" "$(as admin GET "/api/resource/OAuth%20Bearer%20Token?filters=$(jq -rn --arg u "$A" '[["user","=",$u],["status","=","Active"]]|tojson|@uri')" | jq '.data|length')" 0
read -r st code <<<"$(authorize a "$WMS")"
check "signing in to the WMS refused now" "$st/$code" "403/-"
ch=$(curl -sS -H "$SVC" -G "$ERP/api/method/kgarden.access.api.access_changes" --data-urlencode app_code=WMS --data-urlencode "since=$since" | jq -c --arg u "$A" '[.message[] | select(.user==$u)][0]')
check "access_changes tells the WMS" "$(jq -r .granted <<<"$ch")" false

echo "7. The ERPNext desk is an app too: its role brings the ERPNext roles"
as admin PUT "/api/resource/User/$A" '{"kg_app_access":[{"app":"ERPNEXT","app_role":"accounting"}]}' >/dev/null
u=$(as admin GET "/api/resource/User/$A")
check "accounting gets Accounts User" "$(jq -r '[.data.roles[].role] | index("Accounts User") != null' <<<"$u")" true
check "and the desk" "$(jq -r .data.user_type <<<"$u")" "System User"
as admin PUT "/api/resource/User/$A" '{"kg_app_access":[]}' >/dev/null
u=$(as admin GET "/api/resource/User/$A")
check "taken away: role removed" "$(jq -r '[.data.roles[].role] | index("Accounts User") == null' <<<"$u")" true
check "and no desk again" "$(jq -r .data.user_type <<<"$u")" "Website User"

echo "8. A super admin opens every app as admin"
as admin POST /api/resource/User "$(jq -cn --arg e "$B" '{email:$e, first_name:"ผู้ดูแลสูงสุด ทดสอบ", send_welcome_email:0, kg_superadmin:1, kg_employee_code:("SA" + ($e | split(".")[2] | split("@")[0]))}')" >/dev/null
set_password "$B" "$PW_B"
[[ $(login b "$B" "$PW_B") != "" ]]
read -r st code <<<"$(authorize b "$CERP")"
check "super admin signs in to ConstructionERP" "$st" 302
check "as superadmin" "$(userinfo "$(token "$CERP" "$code")" | jq -r .kg_role)" superadmin
check "and has the ERPNext desk" "$(as admin GET "/api/resource/User/$B" | jq -r .data.user_type)" "System User"

echo "9. Tablet PINs are set from the User form by a super admin"
check "PIN status shows it is set" "$(as admin GET "/api/method/kgarden.access.api.pin_status?user=$(jq -rn --arg u "$A" '$u|@uri')" | jq -c '.message|[.pin_set, (.set_on|length>0), .set_by]')" '[true,true,"Administrator"]'
check "never the PIN or its hash" "$(as admin GET "/api/method/kgarden.access.api.pin_status?user=$(jq -rn --arg u "$A" '$u|@uri')" | jq -c '.message|keys')" '["pin_set","set_by","set_on","user"]'
check "no PIN without an employee code" "$(as admin POST /api/method/kgarden.access.api.set_pin "$(jq -cn --arg u "$C" '{user:$u, pin:"1234"}')" | jq -r .exc_type)" ValidationError
check "only a super admin sees PIN status" "$(as c GET "/api/method/kgarden.access.api.pin_status?user=$(jq -rn --arg u "$A" '$u|@uri')" | jq -r .exc_type)" PermissionError

echo "10. Everyone signs in with their employee code and password"
check "sign in with the employee code" "$(login code "E2E$RUN" "$PW_A")" "No App"
check "wrong password refused" "$(login bad "E2E$RUN" "wrong-$RUN")" "Invalid login credentials"
check "the code is the login name" "$(as admin GET "/api/resource/User/$A" | jq -r .data.username)" "E2E$RUN"
check "access without an employee code refused" "$(as admin POST /api/resource/User "$(jq -cn --arg e "e2e.nocode.$RUN@example.com" \
	'{email:$e, first_name:"ไม่มีรหัส", send_welcome_email:0, kg_app_access:[{app:"MES_FLOOR", app_role:"operator"}]}')" | jq -r .exc_type)" ValidationError
check "login page asks for the employee code" "$(curl -sS "$ERP/login" | grep -c 'รหัสพนักงาน</label>')" 1

echo "ALL $pass CHECKS PASSED"
