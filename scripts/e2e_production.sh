#!/usr/bin/env bash
# End-to-end test of the Production (MES) slice against a running ERPNext and MES service.
#
#   ERPNext Work Order -> MES sync -> floor reports output -> outbox -> Manufacture Stock Entry in ERPNext
#
# Checks: stock moves by exactly the reported quantity, a retried tablet request is not counted twice,
# over-reporting is refused, and a worker that "crashes" after ERPNext accepted the entry does not
# post it again. Safe to re-run: every run uses its own Work Order and compares before/after.
#
# Usage: set -a; . ./.env; set +a; scripts/e2e_production.sh
set -euo pipefail

MES=${MES_URL:-http://${MES_HTTP_ADDR:?}}
AUTH="Authorization: token ${ERPNEXT_API_KEY:?}:${ERPNEXT_API_SECRET:?}"
ERP=${ERPNEXT_URL:?}
# K Garden Work Orders backflush from WIP (kgarden app policy): in real use the WMS moves raw
# material there when the floor cuts it; this test receives it straight into WIP.
RAW_WH="${WMS_BRIDGE_WIP_WAREHOUSE:-งานที่กำลังดำเนินการ - KG}"
FG_WH="สินค้าสำเร็จรูป - KG"
RUN=$(date +%s)
pass=0

erp() { # method path [json]
	curl -sS -X "$1" -H "$AUTH" -H "Content-Type: application/json" "$ERP$2" ${3:+--data "$3"}
}
mes() { # method path [json] [idempotency-key]
	curl -sS -X "$1" -H "Content-Type: application/json" ${4:+-H "Idempotency-Key: $4"} "$MES$2" ${3:+--data "$3"} -w '\n%{http_code}'
}
bin_qty() { # item warehouse
	local f
	f=$(jq -cn --arg i "$1" --arg w "$2" '[["item_code","=",$i],["warehouse","=",$w]]')
	erp GET "/api/resource/Bin?fields=%5B%22actual_qty%22%5D&filters=$(jq -rn --arg f "$f" '$f|@uri')" | jq -r '.data[0].actual_qty // 0'
}
se_count() { # stock entries carrying any K Garden key for this work order
	local f
	f=$(jq -cn --arg wo "$1" '[["work_order","=",$wo],["kg_idempotency_key","is","set"],["docstatus","=",1]]')
	erp GET "/api/resource/Stock%20Entry?filters=$(jq -rn --arg f "$f" '$f|@uri')&limit_page_length=100" | jq '.data|length'
}
check() { # description actual expected
	if [[ "$2" == "$3" ]]; then
		echo "  PASS  $1 ($2)"
		pass=$((pass + 1))
	else
		echo "  FAIL  $1: got '$2', expected '$3'"
		exit 1
	fi
}
status_of() { tail -n1 <<<"$1"; }
body_of() { sed '$d' <<<"$1"; }
wait_outbox_idle() {
	for _ in $(seq 1 30); do
		[[ $(mes GET "/outbox?status=pending" | sed '$d' | jq '.data|length') == 0 ]] && return 0
		sleep 1
	done
	echo "outbox did not drain"; exit 1
}
num() { jq -n "($1) + 0"; } # evaluate and normalise numbers (7.0 -> 7)

echo "1. Raw materials for 10 planter sets arrive in WIP ($RAW_WH)"
receipt=$(jq -cn --arg wh "$RAW_WH" '{
	stock_entry_type: "Material Receipt", purpose: "Material Receipt", company: "K Garden", docstatus: 1,
	items: [
		{item_code: "RM-WOOD-PANEL", qty: 40, basic_rate: 45},
		{item_code: "RM-SCREW", qty: 160, basic_rate: 0.5},
		{item_code: "RM-WOOD-STAIN", qty: 2, basic_rate: 320},
		{item_code: "RM-DRIP-TRAY", qty: 10, basic_rate: 35},
		{item_code: "RM-PEAT-MOSS", qty: 30, basic_rate: 28},
		{item_code: "RM-COMPOST", qty: 15, basic_rate: 12},
		{item_code: "RM-PERLITE", qty: 5, basic_rate: 60}
	] | map(. + {t_warehouse: $wh})}')
erp POST "/api/resource/Stock%20Entry" "$receipt" | jq -r '"  receipt " + .data.name'

peat_before=$(bin_qty RM-PEAT-MOSS "$RAW_WH")
fg_before=$(bin_qty FG-PLANTER-SET "$FG_WH")

echo "2. Planner submits a Work Order for 10 sets in ERPNext"
# REST inserts do not run the form's "fetch default BOM" script, so look it up explicitly.
bom_filter=$(jq -rn '[["item","=","FG-PLANTER-SET"],["is_default","=",1],["docstatus","=",1]]|tojson|@uri')
BOM=$(erp GET "/api/resource/BOM?filters=$bom_filter" | jq -r '.data[0].name')
wo_doc=$(jq -cn --arg src "$RAW_WH" --arg fg "$FG_WH" --arg bom "$BOM" '{
	production_item: "FG-PLANTER-SET", bom_no: $bom, qty: 10, company: "K Garden", use_multi_level_bom: 1, skip_transfer: 1,
	source_warehouse: $src, fg_warehouse: $fg, docstatus: 1}')
WO=$(erp POST "/api/resource/Work%20Order" "$wo_doc" | jq -r '.data.name')
echo "  work order $WO"

echo "3. MES syncs and the floor starts the job"
mes POST /sync/work-orders | sed '$d' | jq -c .
check "work order visible in MES" "$(mes GET /work-orders | sed '$d' | jq --arg wo "$WO" '[.data[]|select(.name==$wo)]|length')" 1
check "start operation" "$(status_of "$(mes POST "/work-orders/$WO/start" '{"operator":"สมชาย"}')")" 201
check "second start is refused" "$(status_of "$(mes POST "/work-orders/$WO/start" '{"operator":"สมชาย"}')")" 409

echo "4. Floor reports output"
check "report 4 good" "$(status_of "$(mes POST "/work-orders/$WO/output" '{"good_qty":4,"operator":"สมชาย"}' "e2e-$RUN-a")")" 201
r=$(mes POST "/work-orders/$WO/output" '{"good_qty":4,"operator":"สมชาย"}' "e2e-$RUN-a")
check "tablet retry with same key is a replay" "$(status_of "$r")/$(body_of "$r" | jq .replayed)" "200/true"
check "report 3 good + 1 scrap" "$(status_of "$(mes POST "/work-orders/$WO/output" '{"good_qty":3,"scrap_qty":1,"scrap_reason":"ไม้แตก","operator":"สมชาย"}' "e2e-$RUN-b")")" 201
check "over-report (5 > 3 left) is refused" "$(status_of "$(mes POST "/work-orders/$WO/output" '{"good_qty":5,"operator":"สมชาย"}' "e2e-$RUN-c")")" 409
check "stop operation" "$(status_of "$(mes POST "/work-orders/$WO/stop" '{"operator":"สมชาย"}')")" 200

echo "5. Outbox worker posts to ERPNext"
wait_outbox_idle
check "2 Manufacture entries in ERPNext" "$(se_count "$WO")" 2
check "Work Order produced_qty" "$(num "$(erp GET "/api/resource/Work%20Order/$WO" | jq .data.produced_qty)")" 7
check "finished goods +7" "$(num "$(bin_qty FG-PLANTER-SET "$FG_WH") - $fg_before")" 7
check "peat moss -21 kg (7 sets x 3 kg)" "$(num "$peat_before - $(bin_qty RM-PEAT-MOSS "$RAW_WH")")" 21

echo "6. Worker crash after ERPNext accepted: event is resent but must not post twice"
psql "${MES_DB_OWNER_URL:?}" -qtAc "UPDATE mes.outbox_event SET status = 'pending', next_attempt_at = now()
	WHERE payload->>'work_order' = '$WO'" >/dev/null
wait_outbox_idle
check "still 2 Manufacture entries" "$(se_count "$WO")" 2
check "finished goods still +7" "$(num "$(bin_qty FG-PLANTER-SET "$FG_WH") - $fg_before")" 7

echo "7. MES records what ERPNext consumed"
consumed=$(psql "$MES_DB_OWNER_URL" -qtAc "SELECT sum(c.qty)::numeric(10,3) FROM mes.material_consumption c
	JOIN mes.production_output o ON o.id = c.output_id WHERE o.work_order = '$WO' AND c.item_code = 'RM-PEAT-MOSS'")
check "peat moss consumption mirrored" "$(num "$consumed")" 21

echo "ALL $pass CHECKS PASSED (work order $WO)"
