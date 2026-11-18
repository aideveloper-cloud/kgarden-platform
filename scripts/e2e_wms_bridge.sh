#!/usr/bin/env bash
# End-to-end test of the WMS -> ERPNext bridge against a local WMS copy, ERPNext and the bridge.
#
#   WMS InventoryPostingService -> stock_ledgers (MySQL) -> wms-bridge -> Stock Entries in ERPNext
#
# Checks: every ledger row becomes exactly one submitted Stock Entry, ERPNext stock per item and
# warehouse equals the WMS ledger, nothing is left in transit, and resending everything posts nothing new.
#
# Usage: set -a; . ./.env; set +a; WMS_APP_DIR=~/Desktop/production-pos/point-of-sales scripts/e2e_wms_bridge.sh
# Never point WMS_APP_DIR at a production WMS: the scenario posts stock movements.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WMS_APP_DIR=${WMS_APP_DIR:?path to the production-pos Laravel app}
BRIDGE=http://${WMS_BRIDGE_HTTP_ADDR:?}
AUTH="Authorization: token ${ERPNEXT_API_KEY:?}:${ERPNEXT_API_SECRET:?}"
ERP=${ERPNEXT_URL:?}
TRANSIT=${WMS_BRIDGE_TRANSIT_WAREHOUSE:?}
pass=0

check() {
	if [[ "$2" == "$3" ]]; then echo "  PASS  $1 ($2)"; pass=$((pass + 1)); else echo "  FAIL  $1: got '$2', expected '$3'"; exit 1; fi
}
pg() { psql "${MES_DB_OWNER_URL:?}" -qtAc "$1"; }
se_count() {
	curl -sS -H "$AUTH" "$ERP/api/resource/Stock%20Entry?filters=$(jq -rn '[["kg_idempotency_key","like","wms-ledger-%"],["docstatus","=",1]]|tojson|@uri')&limit_page_length=100000" | jq '.data|length'
}
wait_idle() { # until the bridge has read up to ledger id $1 and nothing is pending
	for _ in $(seq 1 60); do
		s=$(curl -sS "$BRIDGE/summary")
		[[ $(pg "SELECT coalesce(max(ledger_id),0) FROM wms_bridge.ledger_event") -ge $1 && $(jq .events.pending <<<"$s") == 0 ]] && return 0
		sleep 1
	done
	echo "bridge did not catch up"; exit 1
}
# ERPNext Bin qty for every (item, warehouse) the bridge has mapped, vs the ledger sum the bridge read.
diff_stock() {
	local expected actual
	expected=$(pg "SELECT e.sku || '|' || m.erpnext_warehouse || '|' || trim_scale(sum(e.qty))
		FROM wms_bridge.ledger_event e JOIN wms_bridge.warehouse_map m ON m.wms_code = e.wms_warehouse
		GROUP BY e.sku, m.erpnext_warehouse")
	local whs items
	whs=$(pg "SELECT json_agg(erpnext_warehouse) FROM wms_bridge.warehouse_map")
	items=$(pg "SELECT json_agg(DISTINCT sku) FROM wms_bridge.ledger_event")
	actual=$(curl -sS -H "$AUTH" "$ERP/api/resource/Bin?fields=%5B%22item_code%22,%22warehouse%22,%22actual_qty%22%5D&limit_page_length=100000&filters=$(jq -rn --argjson w "$whs" --argjson i "$items" '[["warehouse","in",$w],["item_code","in",$i]]|tojson|@uri')" |
		jq -r '.data[] | select(.actual_qty != 0) | "\(.item_code)|\(.warehouse)|\(.actual_qty)"' | sed -E 's/\.0$//' | sort)
	expected=$(grep -v '|0$' <<<"$expected" | sort || true)
	if [[ "$expected" == "$actual" ]]; then echo same; else diff <(echo "$expected") <(echo "$actual") >&2; echo different; fi
}

echo "1. Post movements in the WMS"
max=$(cd "$WMS_APP_DIR" && php artisan tinker --execute "require '$ROOT/scripts/wms_scenario.php';" | sed -n 's/^LEDGER_MAX=//p')
echo "  WMS ledger now ends at id $max"

echo "2. Bridge copies them to ERPNext"
wait_idle "$max"
check "no dead events" "$(curl -sS "$BRIDGE/summary" | jq .events.dead)" 0
check "one Stock Entry per ledger row" "$(se_count)" "$(pg "SELECT count(*) FROM wms_bridge.ledger_event WHERE status = 'sent'")"
check "ERPNext stock equals WMS ledger, item by warehouse" "$(diff_stock)" same
transit=$(curl -sS -H "$AUTH" "$ERP/api/resource/Bin?fields=%5B%22actual_qty%22%5D&limit_page_length=1000&filters=$(jq -rn --arg w "$TRANSIT" '[["warehouse","=",$w],["item_code","like","RM-%"]]|tojson|@uri')" | jq '[.data[].actual_qty]|add // 0')
check "nothing left in transit" "$(jq -n "$transit + 0")" 0

echo "3. Bridge crashes after ERPNext accepted: every event is resent"
before=$(se_count)
pg "UPDATE wms_bridge.ledger_event SET status = 'pending', next_attempt_at = now()" >/dev/null
wait_idle "$max"
check "no new Stock Entries" "$(se_count)" "$before"
check "stock unchanged" "$(diff_stock)" same

echo "ALL $pass CHECKS PASSED"
