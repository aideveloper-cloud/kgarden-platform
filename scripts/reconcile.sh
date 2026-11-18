#!/usr/bin/env bash
# Read-only reconciliation: does ERPNext (the core) hold exactly what each app says happened?
#
#   WMS (production-pos)  every ledger row posted once; ERPNext stock = WMS ledger, item by warehouse
#   ConstructionERP       every live PO has one submitted ERPNext PO with the same total, and no
#                         submitted ERPNext PO belongs to a draft/cancelled one
#   MES                   every posted output has its submitted Manufacture entry
#
# Reads the WMS and ConstructionERP databases with the bridges' read-only users. Changes nothing.
# Usage: set -a; . ./.env; set +a; scripts/reconcile.sh
set -euo pipefail

ERP=${ERPNEXT_URL:?}
AUTH="Authorization: token ${ERPNEXT_API_KEY:?}:${ERPNEXT_API_SECRET:?}"
PG=${MES_DB_OWNER_URL:?}
fail=0
ok() { echo "  OK    $1"; }
bad() { echo "  DIFF  $1"; fail=1; }
erp_list() { # doctype fields-json filters-json
	curl -sS -H "$AUTH" "$ERP/api/resource/$(jq -rn --arg d "$1" '$d|@uri')?fields=$(jq -rn --arg f "$2" '$f|@uri')&filters=$(jq -rn --arg f "$3" '$f|@uri')&limit_page_length=100000" | jq -c '.data'
}
# WMS read-only DSN user:pw@tcp(host:port)/db -> mysql CLI
wms_sql() {
	local dsn=${WMS_DB_DSN:?} user pw host port db
	user=${dsn%%:*}; pw=${dsn#*:}; pw=${pw%%@tcp(*}
	host=${dsn#*@tcp(}; host=${host%%:*}; port=${dsn#*@tcp(*:}; port=${port%%)*}
	db=${dsn#*)/}; db=${db%%\?*}
	MYSQL_PWD=$pw mysql --default-character-set=utf8mb4 -h "$host" -P "$port" -u "$user" "$db" -N -e "$1" 2> >(grep -v "ssl-verify-server-cert" >&2)
}

echo "== WMS → ERPNext (company K Garden)"
wms_rows=$(wms_sql "SELECT count(*) FROM stock_ledgers")
read -r sent pending dead <<<"$(psql "$PG" -qtAF' ' -c "SELECT count(*) FILTER (WHERE status='sent'), count(*) FILTER (WHERE status='pending'), count(*) FILTER (WHERE status='dead') FROM wms_bridge.ledger_event")"
[[ $sent == "$wms_rows" ]] && ok "all $wms_rows WMS ledger rows posted" || bad "WMS has $wms_rows ledger rows, bridge sent $sent (pending $pending, dead $dead)"
ses=$(erp_list "Stock Entry" '["name"]' '[["kg_idempotency_key","like","wms-ledger-%"],["docstatus","=",1]]' | jq length)
[[ $ses == "$wms_rows" ]] && ok "$ses Stock Entries in ERPNext, one per ledger row" || bad "ERPNext has $ses WMS Stock Entries for $wms_rows ledger rows"
expected=$(wms_sql "SELECT CONCAT(p.sku,'|',w.name,'|',TRIM(TRAILING '.' FROM TRIM(TRAILING '0' FROM CAST(SUM(l.qty) AS CHAR))))
	FROM stock_ledgers l JOIN products p ON p.id=l.product_id JOIN warehouses w ON w.id=l.warehouse_id
	GROUP BY p.sku, w.name HAVING SUM(l.qty) <> 0" | sort)
whs=$(psql "$PG" -qtAc "SELECT json_agg(erpnext_warehouse) FROM wms_bridge.warehouse_map")
actual=$(erp_list Bin '["item_code","warehouse","actual_qty"]' "[[\"warehouse\",\"in\",$whs]]" |
	jq -r '.[] | select(.actual_qty != 0) | "\(.item_code)|\(.warehouse | sub(" - KG$"; ""))|\(.actual_qty)"' | sed -E 's/\.0$//' | sort)
if [[ "$expected" == "$actual" ]]; then ok "stock equal for $(wc -l <<<"$expected" | tr -d ' ') item/warehouse pairs"; else bad "stock differs:"; diff <(echo "$expected") <(echo "$actual") | sed 's/^/        /'; fi

echo "== ConstructionERP → ERPNext (company ${CERP_BRIDGE_COMPANY:-KGF Construction})"
live=$(psql "${CERP_DB_URL:?}" -qtAc "SELECT json_object_agg(po_number, amount_total::numeric) FROM oe_procurement_po
	WHERE status IN ('approved','issued','partially_received','completed')")
live=${live:-{\}}
booked=$(erp_list "Purchase Order" '["kg_cerp_po_number","grand_total","company"]' '[["kg_cerp_po_number","is","set"],["docstatus","=",1]]')
n_live=$(jq length <<<"$live")
missing=$(jq -rn --argjson l "$live" --argjson b "$booked" '$l | keys[] as $k | select(([$b[]|select(.kg_cerp_po_number==$k)]|length) != 1) | $k')
wrong=$(jq -rn --argjson l "$live" --argjson b "$booked" '$b[] | select($l[.kg_cerp_po_number] != null and ($l[.kg_cerp_po_number] - .grand_total | fabs) > 0.005) | .kg_cerp_po_number')
orphan=$(jq -rn --argjson l "$live" --argjson b "$booked" '$b[] | select($l[.kg_cerp_po_number] == null) | .kg_cerp_po_number')
[[ -z $missing ]] && ok "all $n_live live POs have exactly one ERPNext PO" || bad "live POs without exactly one ERPNext PO: $(echo $missing)"
[[ -z $wrong ]] && ok "totals equal to the satang" || bad "total differs: $(echo $wrong)"
[[ -z $orphan ]] && ok "no ERPNext PO for a draft or cancelled PO" || bad "ERPNext PO for a PO that is not live: $(echo $orphan)"
[[ $(jq -r '[.[].company]|unique|join(",")' <<<"$booked") == "${CERP_BRIDGE_COMPANY:-KGF Construction}" || $(jq length <<<"$booked") == 0 ]] &&
	ok "all booked in ${CERP_BRIDGE_COMPANY:-KGF Construction}" || bad "booked in other companies too"

echo "== MES → ERPNext (company K Garden)"
posted=$(psql "$PG" -qtAc "SELECT coalesce(json_agg(stock_entry), '[]') FROM mes.production_output WHERE status = 'posted' AND good_qty > 0")
n_posted=$(jq length <<<"$posted")
found=$(erp_list "Stock Entry" '["name"]' "[[\"name\",\"in\",$posted],[\"docstatus\",\"=\",1],[\"purpose\",\"=\",\"Manufacture\"]]" | jq length)
[[ $found == "$n_posted" ]] && ok "all $n_posted posted outputs have a submitted Manufacture entry" || bad "$n_posted posted outputs, $found Manufacture entries found"
stuck=$(psql "$PG" -qtAc "SELECT count(*) FROM mes.outbox_event WHERE status = 'dead'")
[[ $stuck == 0 ]] && ok "no dead outbox events" || bad "$stuck dead MES outbox events"

echo
[[ $fail == 0 ]] && echo "RECONCILED: ERPNext matches every app" || { echo "DIFFERENCES FOUND"; exit 1; }
