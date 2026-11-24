#!/usr/bin/env bash
# End-to-end test of the production POS (decided 2026-11-24: the shop floor lives in the WMS).
#
#   WMS RM "ตัด" -> (bridge) transfer to WIP
#   -> WMS production POS reports output against an ERPNext Work Order: FG received into the WMS warehouse
#   -> (bridge) Manufacture Stock Entry: BOM backflushed from WIP, FG into that WMS warehouse
#
# Checks: materials leave ERPNext once (WIP back to start), finished goods are counted once and land
# in the WMS warehouse, output reported before its material reached WIP waits instead of failing,
# a retried report is not counted twice, and more than the Work Order quantity is refused.
#
# Needs: local ERPNext with kgarden, wms-bridge serve, WMS app on its local MySQL.
# Usage: set -a; . ./.env; set +a; WMS_APP_DIR=~/Desktop/production-pos/point-of-sales scripts/e2e_pos_production.sh
# Never point WMS_APP_DIR at a production WMS.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WMS_APP_DIR=${WMS_APP_DIR:?path to the production-pos Laravel app}
AUTH="Authorization: token ${ERPNEXT_API_KEY:?}:${ERPNEXT_API_SECRET:?}"
ERP=${ERPNEXT_URL:?}
WIP=${WMS_BRIDGE_WIP_WAREHOUSE:?}
WMS_WH=WH-DECHA-POST
RUN=$(date +%s)
pass=0

check() {
	if [[ "$2" == "$3" ]]; then echo "  PASS  $1 ($2)"; pass=$((pass + 1)); else echo "  FAIL  $1: got '$2', expected '$3'"; exit 1; fi
}
num() { jq -n "($1) + 0"; }
erp() { curl -sS -X "$1" -H "$AUTH" -H "Content-Type: application/json" "$ERP$2" ${3:+--data "$3"}; }
pg() { psql "${MES_DB_OWNER_URL:?}" -qtAc "$1"; }
tinker() { (cd "$WMS_APP_DIR" && php artisan tinker --execute "$1"); }
bin_qty() { # item erpnext-warehouse
	erp GET "/api/resource/Bin?fields=%5B%22actual_qty%22%5D&filters=$(jq -rn --arg i "$1" --arg w "$2" '[["item_code","=",$i],["warehouse","=",$w]]|tojson|@uri')" | jq '.data[0].actual_qty // 0'
}
wait_sent() { # ledger-id
	for _ in $(seq 1 90); do
		[[ $(pg "SELECT count(*) FROM wms_bridge.ledger_event WHERE ledger_id = $1 AND status = 'sent'") == 1 ]] && return 0
		sleep 1
	done
	echo "bridge did not post ledger $1: $(pg "SELECT last_error FROM wms_bridge.ledger_event WHERE ledger_id = $1")"; exit 1
}
wms() { # kind sku qty -> waits until the bridge has posted it
	local max
	max=$(tinker "require '$ROOT/scripts/wms_post.php'; wms_post('$1', '$2', '$WMS_WH', $3);" | sed -n 's/^LEDGER_MAX=//p')
	wait_sent "$max"
}
pos_lists() { # work-order -> 1 when the production POS (WMS service account) lists it as open
	tinker "echo 'N=', collect(app(App\\Services\\KGarden\\WorkOrderClient::class)->open())->where('name', '$1')->count();" | sed -n 's/^N=//p'
}
report() { # work-order good key -> prints OUTPUT/LEDGER/REFUSED lines
	tinker "require '$ROOT/scripts/wms_production.php'; wms_report('$1', '$WMS_WH', $2, 0, '', '$3');"
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
			purpose:"Material Issue",company:"K Garden",docstatus:1,remarks:"e2e_pos_production: clear test leftovers from WIP",
			items:[{item_code:$i,qty:$q,s_warehouse:$w}]}')" | jq -r '"  cleared " + .data.name'
	fi
done
pipe_wip0=$(bin_qty RM-PIPE "$WIP"); plate_wip0=$(bin_qty RM-PLATE "$WIP")
fg_wms0=$(bin_qty FG-POST "$WMS_ERP_WH")
wms_fg0=$(tinker "echo 'FG=', App\Models\StockLedger::whereHas('product', fn(\$q)=>\$q->where('sku','FG-POST'))->sum('qty');" | sed -n 's/^FG=//p')

echo "1. RM POS cuts raw material: it moves to WIP in ERPNext"
wms cut RM-PIPE 18
wms cut RM-PLATE 10
check "pipes in WIP +18" "$(num "$(bin_qty RM-PIPE "$WIP") - $pipe_wip0")" 18
check "plates in WIP +10" "$(num "$(bin_qty RM-PLATE "$WIP") - $plate_wip0")" 10

echo "2. Planner opens a Work Order for 10 posts"
WO=$(erp POST "/api/resource/Work%20Order" "$(jq -cn --arg bom "$BOM" '{production_item:"FG-POST",bom_no:$bom,qty:10,company:"K Garden",docstatus:1}')" | jq -r .data.name)
echo "  work order $WO"
check "production POS sees the open Work Order" "$(pos_lists "$WO")" 1

echo "3. Production POS reports 6 posts: FG into the WMS warehouse, ERPNext backflushes 12 pipes + 6 plates"
out=$(report "$WO" 6 "prd-$RUN-a")
l1=$(sed -n 's/^LEDGER=//p' <<<"$out"); [[ -n "$l1" ]] || { echo "report refused: $out"; exit 1; }
wait_sent "$l1"
se1=$(pg "SELECT stock_entry FROM wms_bridge.ledger_event WHERE ledger_id = $l1")
check "posted as a Manufacture entry" "$(erp GET "/api/resource/Stock%20Entry/$se1" | jq -r '.data.purpose + " " + .data.work_order')" "Manufacture $WO"
check "pipes in WIP 18 - 12" "$(num "$(bin_qty RM-PIPE "$WIP") - $pipe_wip0")" 6
check "plates in WIP 10 - 6" "$(num "$(bin_qty RM-PLATE "$WIP") - $plate_wip0")" 4
check "6 posts in the WMS warehouse in ERPNext" "$(num "$(bin_qty FG-POST "$WMS_ERP_WH") - $fg_wms0")" 6
check "Work Order produced qty" "$(num "$(erp GET "/api/resource/Work%20Order/$WO" | jq .data.produced_qty)")" 6

echo "4. Same report sent again (dropped connection): counted once"
again=$(report "$WO" 6 "prd-$RUN-a")
check "retry returns the first report" "$(sed -n 's/^LEDGER=//p' <<<"$again")" "$l1"

echo "5. More than the Work Order quantity is refused"
check "5 more refused (4 left)" "$(report "$WO" 5 "prd-$RUN-x" | grep -c '^REFUSED=')" 1

echo "6. 4 more reported, but only 6 of the 8 pipes are in WIP: the Manufacture entry waits"
l2=$(report "$WO" 4 "prd-$RUN-b" | sed -n 's/^LEDGER=//p')
for _ in $(seq 1 60); do [[ -n $(pg "SELECT last_error FROM wms_bridge.ledger_event WHERE ledger_id = $l2 AND last_error <> ''") ]] && break; sleep 1; done
check "waiting on stock, not dead" "$(pg "SELECT status || ' ' || (last_error LIKE '%NegativeStock%')::text FROM wms_bridge.ledger_event WHERE ledger_id = $l2")" "pending true"
wms cut RM-PIPE 2
wait_sent "$l2"
check "WIP pipes back to start (cut 20, used 20)" "$(num "$(bin_qty RM-PIPE "$WIP") - $pipe_wip0")" 0
check "WIP plates back to start (cut 10, used 10)" "$(num "$(bin_qty RM-PLATE "$WIP") - $plate_wip0")" 0
check "10 posts in the WMS warehouse in ERPNext" "$(num "$(bin_qty FG-POST "$WMS_ERP_WH") - $fg_wms0")" 10
check "WMS ledger +10 posts (no second FG receive)" "$(num "$(tinker "echo 'FG=', App\Models\StockLedger::whereHas('product', fn(\$q)=>\$q->where('sku','FG-POST'))->sum('qty');" | sed -n 's/^FG=//p') - $wms_fg0")" 10
check "Work Order completed" "$(erp GET "/api/resource/Work%20Order/$WO" | jq -r .data.status)" Completed
check "completed Work Order leaves the POS list" "$(pos_lists "$WO")" 0

echo "ALL $pass CHECKS PASSED (work order $WO)"
