#!/usr/bin/env bash
# End-to-end test of ConstructionERP accounts payable booked in ERPNext (company KGF Construction):
#
#   PO approved/issued -> goods receipt confirmed -> Purchase Receipt
#   invoice from the PO approved -> Purchase Invoice (same total, billed against the PO)
#   payment with Thai WHT (หัก ณ ที่จ่าย) -> Payment Entry: cash + WHT deduction, invoice fully settled
#
# Also: an invoice without a PO, cancelled after approval, leaves no live Purchase Invoice; a receipt
# dated in the future waits instead of failing; a resend of everything duplicates nothing.
#
# Usage: set -a; . ./.env; set +a; CERP_URL=http://127.0.0.1:8200 scripts/e2e_cerp_ap.sh
# Never point CERP_URL at production: it creates projects, contacts, POs, invoices and payments.
set -euo pipefail

CERP=${CERP_URL:?ConstructionERP base URL}
BRIDGE=http://${CERP_BRIDGE_HTTP_ADDR:?}
AUTH="Authorization: token ${CERP_ERPNEXT_API_KEY:?}:${CERP_ERPNEXT_API_SECRET:?}"
ERP=${ERPNEXT_URL:?}
WHT_ACC=${CERP_BRIDGE_WHT_ACCOUNT:?}
TODAY=$(TZ=Asia/Bangkok date +%F)
RUN=$(date +%s)
pass=0

check() {
	if [[ "$2" == "$3" ]]; then echo "  PASS  $1 ($2)"; pass=$((pass + 1)); else echo "  FAIL  $1: got '$2', expected '$3'"; exit 1; fi
}
TOKEN=$(curl -sS -X POST "$CERP/api/v1/users/auth/demo-login/" -H 'Content-Type: application/json' \
	-d '{"email":"demo@openconstructionerp.com"}' | jq -r .access_token)
cerp() { curl -sS -X "$1" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$CERP/api/v1$2" ${3:+--data "$3"}; }
erp() { curl -sS -H "$AUTH" "$ERP$1"; }
must() { # description json-with-maybe-detail field
	local v; v=$(jq -r "$2" <<<"$1")
	[[ -n "$v" && "$v" != null ]] || { echo "ConstructionERP refused $3: $(jq -c '.detail' <<<"$1" | head -c 300)"; exit 1; }
	echo "$v"
}
doc() { # kind cerp-id -> "status erpnext_doc"
	curl -sS "$BRIDGE/documents" | jq -r --arg k "$1" --arg id "$2" '.data[]|select(.kind==$k and .id==$id)|"\(.sync_status) \(.erpnext_doc)"'
}
wait_doc() { # kind cerp-id -> erpnext doc name once synced
	for _ in $(seq 1 60); do
		read -r st name <<<"$(doc "$1" "$2")"
		[[ "$st" == synced ]] && { echo "$name"; return 0; }
		[[ "$st" == dead ]] && { echo "dead: $(curl -sS "$BRIDGE/documents" | jq -r --arg id "$2" '.data[]|select(.id==$id)|.last_error')" >&2; exit 1; }
		sleep 1
	done
	echo "$1 $2 not synced: $(curl -sS "$BRIDGE/documents" | jq -r --arg id "$2" '.data[]|select(.id==$id)|.last_error')" >&2; exit 1
}
wait_po() {
	for _ in $(seq 1 60); do
		[[ $(curl -sS "$BRIDGE/purchase-orders" | jq -r --arg id "$1" '.data[]|select(.id==$id)|"\(.sync_status) \(.cerp_status)"') == "synced $2" ]] && return 0
		sleep 1
	done
	echo "PO $1 not synced as $2"; exit 1
}

echo "0. Project and supplier"
PROJECT=$(must "$(cerp POST /projects/ "$(jq -cn --arg c "KGF-AP$RUN" '{name:"โครงการทดสอบเจ้าหนี้", project_code:$c, currency:"THB", locale:"th", region:"TH", country_code:"TH"}')")" .id project)
VENDOR=$(must "$(cerp POST /contacts/ "$(jq -cn --arg t "01077$RUN" '{contact_type:"supplier", company_name:("บริษัท รับเหมาทดสอบ " + $t), vat_number:$t, country_code:"TH"}')")" .id contact)

echo "1. PO approved and issued (VAT 7% exclusive, WHT 3% on labour)"
po=$(cerp POST /procurement/ "$(jq -cn --arg p "$PROJECT" --arg v "$VENDOR" --arg d "$TODAY" '{project_id:$p, vendor_contact_id:$v, issue_date:$d,
	currency_code:"THB", vat_mode:"exclusive", vat_rate:"7", wht_type:"hire_of_work", wht_rate:"3",
	items:[{description:"ปูนซีเมนต์ 50 กก.", quantity:"100", unit:"ถุง", unit_rate:"150", amount:"15000", cost_category:"material"},
	       {description:"ค่าแรงเทพื้น", quantity:"1", unit:"งาน", unit_rate:"20000", amount:"20000", cost_category:"labor", wht_applies:true}]}')")
PO=$(must "$po" .id po); PO_TOTAL=$(jq -r .amount_total <<<"$po"); PO_WHT=$(jq -r .wht_amount <<<"$po")
must "$(cerp PATCH "/procurement/$PO" '{"status":"approved"}')" .status approve >/dev/null
must "$(cerp POST "/procurement/$PO/issue/")" .status issue >/dev/null
wait_po "$PO" issued

echo "2. Goods received in full: Purchase Receipt against the ERPNext PO"
items=$(cerp GET "/procurement/$PO" | jq -c '[.items[] | {po_item_id: .id, quantity_ordered: .quantity, quantity_received: .quantity}]')
gr=$(cerp POST /procurement/goods-receipts/ "$(jq -cn --arg po "$PO" --arg d "$TODAY" --argjson it "$items" '{po_id:$po, receipt_date:$d, delivery_note_number:"DN-E2E", items:$it}')")
GR=$(must "$gr" .id receipt)
must "$(cerp POST "/procurement/goods-receipts/$GR/confirm/")" .status "confirm receipt" >/dev/null
PR=$(wait_doc receipt "$GR")
erp_po=$(erp "/api/resource/Purchase%20Receipt/$PR" | jq -r '.data.items[0].purchase_order')
check "receipt is against the booked PO" "$(erp "/api/resource/Purchase%20Order/$erp_po" | jq -r '.data.kg_cerp_po_number' | grep -c PO)" 1
check "PO fully received in ERPNext" "$(erp "/api/resource/Purchase%20Order/$erp_po" | jq '.data.per_received + 0')" 100

echo "3. Invoice from the PO approved: Purchase Invoice with the same total, billed against the PO"
INV=$(must "$(cerp POST "/procurement/$PO/create-invoice/")" .invoice_id invoice)
must "$(cerp POST "/finance/$INV/approve/")" .status "approve invoice" >/dev/null
PI=$(wait_doc invoice "$INV")
pi=$(erp "/api/resource/Purchase%20Invoice/$PI")
check "invoice total equals ConstructionERP" "$(jq '.data.grand_total + 0' <<<"$pi")" "$(jq -n "$PO_TOTAL + 0")"
check "billed against the PO" "$(erp "/api/resource/Purchase%20Order/$erp_po" | jq '.data.per_billed + 0')" 100

echo "4. Paid with WHT: cash to the supplier, WHT to the WHT payable account"
cash=$(jq -n "$PO_TOTAL - $PO_WHT")
pay=$(cerp POST "/finance/invoices/$INV/record-payment/" "$(jq -cn --arg d "$TODAY" --arg c "$cash" --arg w "$PO_WHT" --arg r "$RUN" '{payment_date:$d,
	amount:($c|tostring), withholding_amount:"0", currency_code:"THB", wht_type:"hire_of_work", wht_rate:"3", wht_amount:$w,
	wht_certificate_no:("WHT-" + $r), idempotency_key:("e2e-pay-" + $r)}')")
PAY=$(must "$pay" .id payment)
must "$(cerp POST "/finance/$INV/pay/")" .status "mark paid" >/dev/null
PE=$(wait_doc payment "$PAY")
check "invoice fully settled in ERPNext" "$(erp "/api/resource/Purchase%20Invoice/$PI" | jq '.data.outstanding_amount + 0')" 0
gl=$(erp "/api/resource/GL%20Entry?fields=%5B%22account%22,%22debit%22,%22credit%22%5D&filters=$(jq -rn --arg v "$PE" '[["voucher_no","=",$v],["is_cancelled","=",0]]|tojson|@uri')")
check "WHT payable credited" "$(jq --arg a "$WHT_ACC" '[.data[]|select(.account==$a)|.credit]|add + 0' <<<"$gl")" "$(jq -n "$PO_WHT + 0")"
check "supplier debited the full invoice" "$(jq '[.data[]|.debit]|add + 0' <<<"$gl")" "$(jq -n "$PO_TOTAL + 0")"
check "certificate number kept" "$(erp "/api/resource/Payment%20Entry/$PE" | jq -r '.data.kg_wht_certificate_no')" "WHT-$RUN"

echo "5. Invoice without a PO, approved then cancelled: no live Purchase Invoice"
inv2=$(cerp POST /finance/ "$(jq -cn --arg p "$PROJECT" --arg v "$VENDOR" --arg d "$TODAY" --arg n "AP-E2E-$RUN" '{project_id:$p, contact_id:$v,
	invoice_direction:"payable", invoice_number:$n, invoice_date:$d, currency_code:"THB", amount_subtotal:"5000", tax_amount:"350",
	amount_total:"5350", line_items:[{description:"ค่าขนส่งวัสดุ", quantity:"1", unit:"เที่ยว", unit_rate:"5000", amount:"5000", cost_category:"other"}]}')")
INV2=$(must "$inv2" .id "standalone invoice")
must "$(cerp POST "/finance/$INV2/approve/")" .status "approve invoice 2" >/dev/null
PI2=$(wait_doc invoice "$INV2")
check "standalone invoice total" "$(erp "/api/resource/Purchase%20Invoice/$PI2" | jq '.data.grand_total + 0')" 5350
must "$(cerp PATCH "/finance/$INV2" '{"status":"cancelled"}')" .status "cancel invoice 2" >/dev/null
for _ in $(seq 1 60); do [[ $(doc invoice "$INV2") == "synced " ]] && break; sleep 1; done
check "cancelled in ERPNext too" "$(erp "/api/resource/Purchase%20Invoice/$PI2" | jq '.data.docstatus')" 2

echo "6. A receipt dated in the future waits instead of failing"
po3=$(cerp POST /procurement/ "$(jq -cn --arg p "$PROJECT" --arg v "$VENDOR" --arg d "$TODAY" '{project_id:$p, vendor_contact_id:$v, issue_date:$d,
	currency_code:"THB", vat_mode:"exclusive", vat_rate:"7",
	items:[{description:"ทรายหยาบ", quantity:"5", unit:"คิว", unit_rate:"650", amount:"3250", cost_category:"material"}]}')")
PO3=$(must "$po3" .id "small PO")
must "$(cerp PATCH "/procurement/$PO3" '{"status":"approved"}')" .status approve >/dev/null
must "$(cerp POST "/procurement/$PO3/issue/")" .status issue >/dev/null
items3=$(cerp GET "/procurement/$PO3" | jq -c '[.items[] | {po_item_id: .id, quantity_ordered: .quantity, quantity_received: .quantity}]')
GR2=$(must "$(cerp POST /procurement/goods-receipts/ "$(jq -cn --arg po "$PO3" --argjson it "$items3" '{po_id:$po, receipt_date:"2099-01-01", delivery_note_number:"DN-FUTURE", items:$it}')")" .id "future receipt")
must "$(cerp POST "/procurement/goods-receipts/$GR2/confirm/")" .status "confirm future receipt" >/dev/null
for _ in $(seq 1 30); do [[ -n $(doc receipt "$GR2") ]] && break; sleep 1; done
sleep 5
read -r st _ <<<"$(doc receipt "$GR2")"
check "future receipt waits (not dead, not booked)" "$st" pending
check "it says why" "$(curl -sS "$BRIDGE/documents" | jq -r --arg id "$GR2" '.data[]|select(.id==$id)|.last_error' | grep -c 'waits until')" 1

echo "7. Bridge crash: everything resent, nothing duplicated"
psql "${MES_DB_OWNER_URL:?}" -qtAc "UPDATE cerp_bridge.doc_state SET sync_status = 'pending', next_attempt_at = now() WHERE kind <> 'receipt' OR cerp_id = '$GR'" >/dev/null
sleep 10
count() { erp "/api/resource/$1?filters=$(jq -rn --arg k "$2" '[["kg_idempotency_key","=",$k],["docstatus","=",1]]|tojson|@uri')" | jq '.data|length'; }
check "one Purchase Receipt" "$(count Purchase%20Receipt "cerp-receipt-$GR")" 1
check "one Purchase Invoice" "$(count Purchase%20Invoice "cerp-invoice-$INV")" 1
check "one Payment Entry" "$(count Payment%20Entry "cerp-payment-$PAY")" 1
check "invoice still settled" "$(erp "/api/resource/Purchase%20Invoice/$PI" | jq '.data.outstanding_amount + 0')" 0

echo "ALL $pass CHECKS PASSED"
