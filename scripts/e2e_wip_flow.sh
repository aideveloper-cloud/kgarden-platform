#!/usr/bin/env bash
# End-to-end test of the production flow decided on 2026-11-18:
#
#   WMS "รับเข้า" raw material -> WMS "ตัด" -> (bridge) transfer to WIP
#   -> MES reports output -> ERPNext backflushes the BOM from WIP into the FG output warehouse
#   -> WMS FG "รับเข้า" -> (bridge) transfer from the FG output warehouse into the WMS warehouse
#
# Checks: Work Orders are forced to backflush from WIP, materials leave ERPNext exactly once (WIP ends
# at zero), an output reported before its material reached WIP waits instead of failing, and finished
# goods are not counted twice.
#
# Usage: set -a; . ./.env; set +a; WMS_APP_DIR=~/Desktop/production-pos/point-of-sales scripts/e2e_wip_flow.sh
# Never point WMS_APP_DIR at a production WMS.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WMS_APP_DIR=${WMS_APP_DIR:?path to the production-pos Laravel app}
MES=http://${MES_HTTP_ADDR:?}
BRIDGE=http://${WMS_BRIDGE_HTTP_ADDR:?}
AUTH="Authorization: token ${ERPNEXT_API_KEY:?}:${ERPNEXT_API_SECRET:?}"
ERP=${ERPNEXT_URL:?}
WIP=${WMS_BRIDGE_WIP_WAREHOUSE:?}
FG_STAGE=${WMS_BRIDGE_FG_STAGING_WAREHOUSE:?}
WMS_WH=WH-DECHA-POST
RUN=$(date +%s)
pass=0

check() {
	if [[ "$2" == "$3" ]]; then echo "  PASS  $1 ($2)"; pass=$((pass + 1)); else echo "  FAIL  $1: got '$2', expected '$3'"; exit 1; fi
}
num() { jq -n "($1) + 0"; }
erp() { curl -sS -X "$1" -H "$AUTH" -H "Content-Type: application/json" "$ERP$2" ${3:+--data "$3"}; }
# The MES needs a session: sign in as the test supervisor (employee code + PIN managed in ERPNext).
JAR=$(mktemp); trap 'rm -f "$JAR"' EXIT
curl -sS -c "$JAR" -X POST "$MES/auth/pin" -H 'Content-Type: application/json' \
	--data "$(jq -cn --arg c "${MES_E2E_SUPERVISOR:?}" --arg p "${MES_E2E_SUPERVISOR_PIN:?}" '{employee_code:$c, pin:$p}')" | jq -e .role >/dev/null \
	|| { echo "MES sign-in failed"; exit 1; }
mes() { curl -sS -b "$JAR" -X "$1" -H "Content-Type: application/json" ${4:+-H "Idempotency-Key: $4"} "$MES$2" ${3:+--data "$3"}; }
pg() { psql "${MES_DB_OWNER_URL:?}" -qtAc "$1"; }
bin_qty() { # item erpnext-warehouse
	erp GET "/api/resource/Bin?fields=%5B%22actual_qty%22%5D&filters=$(jq -rn --arg i "$1" --arg w "$2" '[["item_code","=",$i],["warehouse","=",$w]]|tojson|@uri')" | jq '.data[0].actual_qty // 0'
}
wms() { # kind sku qty -> waits until the bridge has handled the new ledger row
	local max
	max=$(cd "$WMS_APP_DIR" && php artisan tinker --execute "require '$ROOT/scripts/wms_post.php'; wms_post('$1', '$2', '$WMS_WH', $3);" | sed -n 's/^LEDGER_MAX=//p')
	for _ in $(seq 1 60); do
		[[ $(pg "SELECT count(*) FROM wms_bridge.ledger_event WHERE ledger_id = $max AND status = 'sent'") == 1 ]] && return 0
		sleep 1
	done
	echo "bridge did not post ledger $max: $(pg "SELECT last_error FROM wms_bridge.ledger_event WHERE ledger_id = $max")"; exit 1
}
mes_output() { # work-order qty key -> output id
	mes POST "/work-orders/$1/output" "{\"good_qty\":$2,\"operator\":\"สมชาย\"}" "$3" | jq -r .output_id
}
output_status() { pg "SELECT status FROM mes.production_output WHERE id = '$1'"; }
wait_output() { # output-id status
	for _ in $(seq 1 90); do [[ $(output_status "$1") == "$2" ]] && return 0; sleep 1; done
	echo "output $1 is $(output_status "$1"), expected $2"; exit 1
}

echo "0. Masters: raw materials known to ERPNext, BOM for FG-POST (2 pipes + 1 plate per post)"
wms receive RM-PIPE 40
wms receive RM-PLATE 20
BOM=$(erp GET "/api/resource/BOM?filters=$(jq -rn '[["item","=","FG-POST"],["is_default","=",1],["docstatus","=",1]]|tojson|@uri')" | jq -r '.data[0].name // empty')
if [[ -z "$BOM" ]]; then
	BOM=$(erp POST "/api/resource/BOM" '{"item":"FG-POST","quantity":1,"company":"K Garden","is_default":1,"is_active":1,"docstatus":1,
		"items":[{"item_code":"RM-PIPE","qty":2},{"item_code":"RM-PLATE","qty":1}]}' | jq -r .data.name)
fi
echo "  BOM $BOM"
WMS_ERP_WH=$(pg "SELECT erpnext_warehouse FROM wms_bridge.warehouse_map WHERE wms_code = '$WMS_WH'")
# Leftovers from earlier runs would hide the "material not in WIP yet" case: issue them out of WIP
# (an ERPNext-only test adjustment; the WMS never sees WIP).
for item in RM-PIPE RM-PLATE; do
	left=$(bin_qty "$item" "$WIP")
	if [[ $(jq -n "$left > 0") == true ]]; then
		erp POST "/api/resource/Stock%20Entry" "$(jq -cn --arg i "$item" --arg w "$WIP" --argjson q "$left" '{stock_entry_type:"Material Issue",
			purpose:"Material Issue",company:"K Garden",docstatus:1,remarks:"e2e_wip_flow: clear test leftovers from WIP",
			items:[{item_code:$i,qty:$q,s_warehouse:$w}]}')" | jq -r '"  cleared " + .data.name'
	fi
done
pipe_wip0=$(bin_qty RM-PIPE "$WIP"); plate_wip0=$(bin_qty RM-PLATE "$WIP")
fg_stage0=$(bin_qty FG-POST "$FG_STAGE"); fg_wms0=$(bin_qty FG-POST "$WMS_ERP_WH")

echo "1. Floor cuts raw material in the WMS: it moves to WIP in ERPNext, not out of stock"
wms cut RM-PIPE 18
wms cut RM-PLATE 10
check "pipes in WIP +18" "$(num "$(bin_qty RM-PIPE "$WIP") - $pipe_wip0")" 18
check "plates in WIP +10" "$(num "$(bin_qty RM-PLATE "$WIP") - $plate_wip0")" 10

echo "2. Planner opens a Work Order for 10 posts (asks for another source warehouse on purpose)"
WO=$(erp POST "/api/resource/Work%20Order" "$(jq -cn --arg bom "$BOM" --arg fg "$FG_STAGE" '{production_item:"FG-POST",bom_no:$bom,qty:10,company:"K Garden",
	skip_transfer:0,source_warehouse:"คลังวัตถุดิบ - KG",fg_warehouse:$fg,docstatus:1}')" | jq -r .data.name)
echo "  work order $WO"
wo=$(erp GET "/api/resource/Work%20Order/$WO")
check "source warehouse forced to WIP" "$(jq -r .data.source_warehouse <<<"$wo")" "$WIP"
check "every material backflushed from WIP" "$(jq -r --arg w "$WIP" '[.data.required_items[]|.source_warehouse==$w]|all' <<<"$wo")" true
check "skip transfer on" "$(jq -r .data.skip_transfer <<<"$wo")" 1
mes POST /sync/work-orders >/dev/null
mes POST "/work-orders/$WO/start" '{"operator":"สมชาย"}' >/dev/null

echo "3. MES reports 6 posts: ERPNext backflushes 12 pipes + 6 plates from WIP"
o1=$(mes_output "$WO" 6 "wip-$RUN-a"); wait_output "$o1" posted
check "pipes in WIP 18 - 12" "$(num "$(bin_qty RM-PIPE "$WIP") - $pipe_wip0")" 6
check "plates in WIP 10 - 6" "$(num "$(bin_qty RM-PLATE "$WIP") - $plate_wip0")" 4

echo "4. MES reports 4 more, but only 6 of the 8 pipes are in WIP: the output waits"
o2=$(mes_output "$WO" 4 "wip-$RUN-b")
for _ in $(seq 1 30); do [[ -n $(pg "SELECT last_error FROM mes.outbox_event WHERE aggregate_id = '$o2' AND last_error <> ''") ]] && break; sleep 1; done
check "output still pending" "$(output_status "$o2")" pending
check "tablet sees why it waits" "$(mes GET "/work-orders/$WO" | jq -r --arg id "$o2" '.outputs[]|select(.id==$id)|.waiting_reason|contains("NegativeStockError")')" true
wms cut RM-PIPE 2
wait_output "$o2" posted
check "output posted once the pipes arrived" "$(output_status "$o2")" posted
check "WIP pipes back to start (cut 20, used 20)" "$(num "$(bin_qty RM-PIPE "$WIP") - $pipe_wip0")" 0
check "WIP plates back to start (cut 10, used 10)" "$(num "$(bin_qty RM-PLATE "$WIP") - $plate_wip0")" 0
check "10 posts in the FG output warehouse" "$(num "$(bin_qty FG-POST "$FG_STAGE") - $fg_stage0")" 10

echo "5. WMS FG tablet receives the 10 posts: moved from the output warehouse, not counted again"
wms receive FG-POST 10
check "FG output warehouse back to start" "$(num "$(bin_qty FG-POST "$FG_STAGE") - $fg_stage0")" 0
check "WMS warehouse +10 posts in ERPNext" "$(num "$(bin_qty FG-POST "$WMS_ERP_WH") - $fg_wms0")" 10

echo "ALL $pass CHECKS PASSED (work order $WO)"
