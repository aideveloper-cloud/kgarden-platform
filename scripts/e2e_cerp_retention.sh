#!/usr/bin/env bash
# End-to-end test of ConstructionERP retention, credit notes and refunds booked in ERPNext
# (company KGF Construction):
#
#   invoice with retention (เงินประกันผลงาน) -> Purchase Invoice for the gross, retention shown on it
#   payment holding the retention back + WHT -> only cash + WHT settle; retention stays owed on the invoice
#   retention released later                 -> an ordinary payment settles what was held
#   paid invoice reversed (credit note)      -> Debit Note returning the whole Purchase Invoice
#   supplier pays it back (refund)           -> Payment Entry, Receive from the supplier, against that balance
#   overpayment, then refunded               -> extra cash left unallocated, the refund clears it
#
# Usage: set -a; . ./.env; set +a; CERP_URL=http://127.0.0.1:8200 scripts/e2e_cerp_retention.sh
# Never point CERP_URL at production: it creates projects, contacts, invoices and payments.
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
	-d "{\"email\":\"${CERP_E2E_DEMO_EMAIL:-manager@openconstructionerp.com}\"}" | jq -r .access_token)
cerp() { curl -sS -X "$1" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$CERP/api/v1$2" ${3:+--data "$3"}; }
erp() { curl -sS -H "$AUTH" "$ERP$1"; }
must() { # json jq-path what
	local v; v=$(jq -r "$2" <<<"$1")
	[[ -n "$v" && "$v" != null ]] || { echo "ConstructionERP refused $3: $(jq -c '.detail' <<<"$1" | head -c 300)"; exit 1; }
	echo "$v"
}
wait_doc() { # kind cerp-id -> erpnext doc name once synced
	local st name
	for _ in $(seq 1 60); do
		read -r st name <<<"$(curl -sS "$BRIDGE/documents?kind=$1" | jq -r --arg id "$2" '.data[]|select(.id==$id)|"\(.sync_status) \(.erpnext_doc)"')"
		[[ "$st" == synced ]] && { echo "$name"; return 0; }
		[[ "$st" == dead ]] && break
		sleep 1
	done
	echo "$1 $2 not synced: $(curl -sS "$BRIDGE/documents?kind=$1" | jq -r --arg id "$2" '.data[]|select(.id==$id)|.last_error')" >&2; exit 1
}
outstanding() { erp "/api/resource/Purchase%20Invoice/$1" | jq '.data.outstanding_amount + 0'; }
gl() { # voucher -> its live GL rows
	erp "/api/resource/GL%20Entry?limit_page_length=50&fields=%5B%22account%22,%22debit%22,%22credit%22,%22party%22%5D&filters=$(jq -rn --arg v "$1" '[["voucher_no","=",$v],["is_cancelled","=",0]]|tojson|@uri')"
}
invoice() { # number subtotal tax retention total
	cerp POST /finance/ "$(jq -cn --arg p "$PROJECT" --arg v "$VENDOR" --arg d "$TODAY" --arg n "$1" --arg s "$2" --arg t "$3" --arg r "$4" --arg g "$5" \
		'{project_id:$p, contact_id:$v, invoice_direction:"payable", invoice_number:$n, invoice_date:$d, currency_code:"THB",
		amount_subtotal:$s, tax_amount:$t, retention_amount:$r, amount_total:$g,
		line_items:[{description:"งานโครงสร้างงวดที่ 1", quantity:"1", unit:"งวด", unit_rate:$s, amount:$s, cost_category:"subcontract"}]}')"
}
payment() { # invoice amount withholding refund? key [wht]
	cerp POST /finance/payments/ "$(jq -cn --arg i "$1" --arg a "$2" --arg h "$3" --argjson r "$4" --arg d "$TODAY" --arg k "$5" --arg w "${6:-0}" \
		'{invoice_id:$i, payment_date:$d, amount:$a, withholding_amount:$h, is_refund:$r, currency_code:"THB", idempotency_key:$k, wht_amount:$w}
		+ (if ($w|tonumber) > 0 then {wht_type:"hire_of_work", wht_rate:"3", wht_certificate_no:("WHT-R-" + $k)} else {} end)')"
}

echo "0. Project and subcontractor"
PROJECT=$(must "$(cerp POST /projects/ "$(jq -cn --arg c "KGF-RT$RUN" '{name:"โครงการทดสอบเงินประกัน", project_code:$c, currency:"THB", locale:"th", region:"TH", country_code:"TH"}')")" .id project)
VENDOR=$(must "$(cerp POST /contacts/ "$(jq -cn --arg t "02077$RUN" '{contact_type:"supplier", company_name:("บริษัท ผู้รับเหมาช่วงทดสอบ " + $t), vat_number:$t, country_code:"TH"}')")" .id contact)

echo "1. Invoice with 5% retention: Purchase Invoice for the gross, retention shown"
INV=$(must "$(invoice "RT-$RUN" 100000 7000 5350 107000)" .id "retention invoice")
must "$(cerp POST "/finance/$INV/approve/")" .status "approve invoice" >/dev/null
PI=$(wait_doc invoice "$INV")
pi=$(erp "/api/resource/Purchase%20Invoice/$PI")
check "invoice gross equals ConstructionERP" "$(jq '.data.grand_total + 0' <<<"$pi")" 107000
check "retention shown on the invoice" "$(jq '.data.kg_retention_amount + 0' <<<"$pi")" 5350

echo "2. Paid holding retention back, with 3% WHT: retention stays owed on the invoice"
# record-payment is the ConstructionERP path that keeps retention held (POST /finance/payments/ drops it).
PAY=$(must "$(cerp POST "/finance/invoices/$INV/record-payment/" "$(jq -cn --arg d "$TODAY" --arg k "rt-pay-$RUN" '{payment_date:$d,
	amount:"98650", withholding_amount:"5350", currency_code:"THB", wht_type:"hire_of_work", wht_rate:"3", wht_amount:"3000",
	wht_certificate_no:("WHT-" + $k), idempotency_key:$k}')")" .id payment)
PE=$(wait_doc payment "$PAY")
pe=$(erp "/api/resource/Payment%20Entry/$PE")
check "cash paid" "$(jq '.data.paid_amount + 0' <<<"$pe")" 98650
check "settles cash + WHT only" "$(jq '.data.total_allocated_amount + 0' <<<"$pe")" 101650
check "retention held recorded on the payment" "$(jq '.data.kg_retention_held + 0' <<<"$pe")" 5350
check "WHT payable credited" "$(gl "$PE" | jq --arg a "$WHT_ACC" '[.data[]|select(.account==$a)|.credit]|add + 0')" 3000
check "retention still owed to the subcontractor" "$(outstanding "$PI")" 5350

echo "3. Retention released: an ordinary payment settles what was held"
REL=$(must "$(payment "$INV" 5350 0 false "rt-release-$RUN")" .id "retention release")
PE2=$(wait_doc payment "$REL")
check "invoice settled after release" "$(outstanding "$PI")" 0
check "release paid in cash" "$(erp "/api/resource/Payment%20Entry/$PE2" | jq '.data.paid_amount + 0')" 5350

echo "4. Paid invoice reversed by credit note: Debit Note returning the whole invoice"
INV2=$(must "$(invoice "CN-$RUN" 20000 1400 0 21400)" .id "invoice to reverse")
must "$(cerp POST "/finance/$INV2/approve/")" .status "approve invoice 2" >/dev/null
PI2=$(wait_doc invoice "$INV2")
PAY2=$(must "$(payment "$INV2" 21400 0 false "cn-pay-$RUN")" .id "payment 2")
wait_doc payment "$PAY2" >/dev/null
must "$(cerp POST "/finance/$INV2/pay/")" .status "mark paid" >/dev/null
must "$(cerp PATCH "/finance/$INV2" '{"status":"credit_note_issued"}')" .status "credit note" >/dev/null
DN=$(wait_doc credit_note "$INV2")
dn=$(erp "/api/resource/Purchase%20Invoice/$DN")
check "debit note is a return" "$(jq -r '.data.is_return' <<<"$dn")" 1
check "returns the invoice" "$(jq -r '.data.return_against' <<<"$dn")" "$PI2"
check "for the whole amount" "$(jq '.data.grand_total + 0' <<<"$dn")" -21400
check "subcontractor now owes it back (on the Debit Note, the invoice was paid)" "$(outstanding "$DN")" -21400

echo "5. Refund from the subcontractor: Receive entry against the credit note balance"
REF=$(must "$(payment "$INV2" 21400 0 true "cn-refund-$RUN")" .id refund)
PE3=$(wait_doc payment "$REF")
pe3=$(erp "/api/resource/Payment%20Entry/$PE3")
check "money received from the supplier" "$(jq -r '.data.payment_type + " " + .data.party_type' <<<"$pe3")" "Receive Supplier"
check "received amount" "$(jq '.data.received_amount + 0' <<<"$pe3")" 21400
check "credit note balance cleared" "$(outstanding "$DN") $(outstanding "$PI2")" "0 0"

echo "6. Overpaid, then the extra refunded: party balance back to zero"
INV3=$(must "$(invoice "OP-$RUN" 1000 70 0 1070)" .id "invoice 3")
must "$(cerp POST "/finance/$INV3/approve/")" .status "approve invoice 3" >/dev/null
PI3=$(wait_doc invoice "$INV3")
OVER=$(must "$(payment "$INV3" 1200 0 false "op-pay-$RUN")" .id overpayment)
PE4=$(wait_doc payment "$OVER")
check "invoice settled" "$(outstanding "$PI3")" 0
check "extra 130 left as an advance" "$(erp "/api/resource/Payment%20Entry/$PE4" | jq '.data.unallocated_amount + 0')" 130
BACK=$(must "$(payment "$INV3" 130 0 true "op-refund-$RUN")" .id "overpayment refund")
PE5=$(wait_doc payment "$BACK")
supplier=$(jq -r .data.supplier <<<"$pi")
bal=$(erp "/api/method/erpnext.accounts.utils.get_balance_on?party_type=Supplier&party=$(jq -rn --arg s "$supplier" '$s|@uri')&company=$(jq -rn --arg c "${CERP_BRIDGE_COMPANY:-KGF Construction}" '$c|@uri')" | jq '.message + 0')
check "subcontractor balance zero after everything" "$bal" 0

echo "7. Bridge crash: everything resent, nothing duplicated"
psql "${MES_DB_OWNER_URL:?}" -qtAc "UPDATE cerp_bridge.doc_state SET sync_status = 'pending', next_attempt_at = now()
	WHERE cerp_id IN ('$INV', '$PAY', '$REL', '$INV2', '$PAY2', '$REF', '$INV3', '$OVER', '$BACK')" >/dev/null
sleep 10
count() { erp "/api/resource/$1?filters=$(jq -rn --arg k "$2" '[["kg_idempotency_key","=",$k],["docstatus","=",1]]|tojson|@uri')" | jq '.data|length'; }
check "one Debit Note" "$(count Purchase%20Invoice "cerp-credit_note-$INV2")" 1
check "one refund entry" "$(count Payment%20Entry "cerp-payment-$REF")" 1
check "one retention release" "$(count Payment%20Entry "cerp-payment-$REL")" 1
check "balances unchanged" "$(outstanding "$PI") $(outstanding "$PI2") $(outstanding "$DN") $(outstanding "$PI3")" "0 0 0 0"

echo "ALL $pass CHECKS PASSED"
