#!/usr/bin/env bash
# End-to-end test of the ConstructionERP -> ERPNext bridge (decided 2026-11-18: ConstructionERP buys,
# ERPNext books, in its own company). Runs against a local ConstructionERP with demo login enabled.
#
# Checks: an approved PO is booked with the same total (VAT exclusive and inclusive), issuing it
# changes nothing, an approved PO sent back to draft and edited is re-booked (old ERPNext PO
# cancelled), a cancelled PO leaves no live ERPNext PO, and a resend never duplicates.
#
# Usage: set -a; . ./.env; set +a; CERP_URL=http://127.0.0.1:8200 scripts/e2e_cerp_bridge.sh
# Never point CERP_URL at production: it creates projects, contacts and POs.
set -euo pipefail

CERP=${CERP_URL:?ConstructionERP base URL}
BRIDGE=http://${CERP_BRIDGE_HTTP_ADDR:?}
AUTH="Authorization: token ${CERP_ERPNEXT_API_KEY:?}:${CERP_ERPNEXT_API_SECRET:?}"
ERP=${ERPNEXT_URL:?}
RUN=$(date +%s)
pass=0

check() {
	if [[ "$2" == "$3" ]]; then echo "  PASS  $1 ($2)"; pass=$((pass + 1)); else echo "  FAIL  $1: got '$2', expected '$3'"; exit 1; fi
}
TOKEN=$(curl -sS -X POST "$CERP/api/v1/users/auth/demo-login/" -H 'Content-Type: application/json' \
	-d "{\"email\":\"${CERP_E2E_DEMO_EMAIL:-manager@openconstructionerp.com}\"}" | jq -r .access_token)
cerp() { curl -sS -X "$1" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$CERP/api/v1$2" ${3:+--data "$3"}; }
erp_pos() { # submitted ERPNext POs for a ConstructionERP PO number -> "name total" lines
	curl -sS -H "$AUTH" "$ERP/api/resource/Purchase%20Order?fields=%5B%22name%22,%22grand_total%22%5D&filters=$(jq -rn --arg n "$1" '[["kg_cerp_po_number","=",$n],["docstatus","=",1]]|tojson|@uri')" |
		jq -r '.data[] | "\(.name) \(.grand_total)"'
}
live_total() { erp_pos "$1" | awk '{print $2}' | xargs -I{} jq -n '{} + 0'; }
wait_synced() { # cerp-po-id
	for _ in $(seq 1 60); do
		s=$(curl -sS "$BRIDGE/purchase-orders" | jq -r --arg id "$1" '.data[]|select(.id==$id)|"\(.sync_status) \(.cerp_status)"')
		[[ "$s" == "synced $2" ]] && return 0
		[[ "$s" == dead* ]] && { curl -sS "$BRIDGE/purchase-orders" | jq -r --arg id "$1" '.data[]|select(.id==$id)|.last_error'; exit 1; }
		sleep 1
	done
	echo "PO $1 not synced as $2"; exit 1
}
status() { # po-id approved|draft|issued|cancelled; issue and cancel are actions of their own
	local r
	case $2 in
	issued) r=$(cerp POST "/procurement/$1/issue/") ;;
	cancelled) r=$(cerp POST "/procurement/$1/cancel/" '{"reason":"e2e: ยกเลิกทดสอบ"}') ;;
	*) r=$(cerp PATCH "/procurement/$1" "{\"status\":\"$2\"}") ;;
	esac
	[[ $(jq -r .status <<<"$r") == "$2" ]] || { echo "ConstructionERP refused $2: $(jq -c .detail <<<"$r" | head -c 400)"; exit 1; }
}
po_body() { # project vendor vat_mode labour-qty
	jq -cn --arg p "$1" --arg v "$2" --arg m "$3" --arg q "$4" '{project_id:$p, vendor_contact_id:$v, issue_date:"2026-11-18",
		currency_code:"THB", vat_mode:$m, vat_rate:"7", wht_type:"hire_of_work", wht_rate:"3",
		items:[{description:"เหล็กเส้น DB12", quantity:"50", unit:"เส้น", unit_rate:"185", amount:"9250", cost_category:"material"},
		       {description:"ค่าแรงผูกเหล็ก", quantity:$q, unit:"วัน", unit_rate:"800", amount:(($q|tonumber)*800|tostring),
		        cost_category:"labor", wht_applies:true}]}'
}

echo "0. Project and supplier in ConstructionERP"
PROJECT=$(cerp POST /projects/ "$(jq -cn --arg c "KGF-T$RUN" '{name:"โครงการทดสอบ bridge", project_code:$c, currency:"THB", locale:"th", region:"TH", country_code:"TH"}')" | jq -r .id)
VENDOR=$(cerp POST /contacts/ "$(jq -cn --arg t "01055$RUN" '{contact_type:"supplier", company_name:("ห้างหุ้นส่วน เหล็กไทย " + $t), vat_number:$t, country_code:"TH"}')" | jq -r .id)

echo "1. Approved PO (VAT exclusive) is booked with the same total"
po=$(cerp POST /procurement/ "$(po_body "$PROJECT" "$VENDOR" exclusive 5)")
A=$(jq -r .id <<<"$po"); A_NO=$(jq -r .po_number <<<"$po"); A_TOTAL=$(jq -r .amount_total <<<"$po")
status "$A" approved; wait_synced "$A" approved
check "one ERPNext PO" "$(erp_pos "$A_NO" | wc -l | tr -d ' ')" 1
check "same total as ConstructionERP" "$(live_total "$A_NO")" "$(jq -n "$A_TOTAL + 0")"
first=$(erp_pos "$A_NO" | awk '{print $1}')

echo "2. Issued to the supplier: same commitment, nothing new in ERPNext"
status "$A" issued; sleep 7
check "still the same ERPNext PO" "$(erp_pos "$A_NO" | awk '{print $1}')" "$first"

echo "3. VAT-inclusive PO is booked with the same total"
po=$(cerp POST /procurement/ "$(po_body "$PROJECT" "$VENDOR" inclusive 2)")
B=$(jq -r .id <<<"$po"); B_NO=$(jq -r .po_number <<<"$po"); B_TOTAL=$(jq -r .amount_total <<<"$po")
status "$B" approved; wait_synced "$B" approved
check "inclusive total matches" "$(live_total "$B_NO")" "$(jq -n "$B_TOTAL + 0")"

echo "4. Approved PO sent back to draft, edited, approved again: ERPNext follows"
before=$(erp_pos "$B_NO" | awk '{print $1}')
status "$B" draft; wait_synced "$B" draft
check "no live ERPNext PO while it is a draft" "$(erp_pos "$B_NO" | wc -l | tr -d ' ')" 0
cerp PATCH "/procurement/$B" "$(po_body "$PROJECT" "$VENDOR" inclusive 4 | jq -c '{items, vat_mode, vat_rate}')" >/dev/null
B_TOTAL=$(cerp GET "/procurement/$B" | jq -r .amount_total)
status "$B" approved; wait_synced "$B" approved
check "exactly one live ERPNext PO" "$(erp_pos "$B_NO" | wc -l | tr -d ' ')" 1
check "it carries the new total" "$(live_total "$B_NO")" "$(jq -n "$B_TOTAL + 0")"
check "it is a new document" "$([[ $(erp_pos "$B_NO" | awk '{print $1}') != "$before" ]] && echo new)" new

echo "5. Cancelled in ConstructionERP: cancelled in ERPNext"
status "$A" cancelled; wait_synced "$A" cancelled
check "no live ERPNext PO for the cancelled one" "$(erp_pos "$A_NO" | wc -l | tr -d ' ')" 0

echo "6. Bridge crash: every PO is processed again, nothing is duplicated"
psql "${MES_DB_OWNER_URL:?}" -qtAc "UPDATE cerp_bridge.po_state SET sync_status = 'pending', next_attempt_at = now()" >/dev/null
wait_synced "$B" approved; sleep 3
check "still one live ERPNext PO for B" "$(erp_pos "$B_NO" | wc -l | tr -d ' ')" 1
check "still none for cancelled A" "$(erp_pos "$A_NO" | wc -l | tr -d ' ')" 0
check "one ERPNext supplier for the vendor" "$(curl -sS -H "$AUTH" "$ERP/api/resource/Supplier?filters=$(jq -rn --arg v "$VENDOR" '[["kg_cerp_id","=",$v]]|tojson|@uri')" | jq '.data|length')" 1

echo "ALL $pass CHECKS PASSED"
