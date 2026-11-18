package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"github.com/jackc/pgx/v5"
)

// settleAfter is how old a WMS ledger row must be before the read cursor may pass it. MySQL
// auto-increment ids become visible in commit order, not id order, so a smaller id can appear
// after a larger one; no WMS posting transaction runs this long. The age is computed here, not in
// MySQL: the WMS writes timestamps in its app timezone (WMS_TIMEZONE) while MySQL NOW() is UTC.
const settleAfter = 5 * time.Minute

const pageSize = 1000

// Reader copies new rows of the WMS stock ledger (read-only MySQL user) into wms_bridge.ledger_event.
type Reader struct {
	wms   *sql.DB
	store *pgstore.Store
}

type ledgerRow struct {
	ID                           int64
	Movement, SKU, Title, Domain string
	WarehouseCode, WarehouseName string
	Qty, UOM, UnitCost, PostedAt string
	RefType, Operator, Notes     string
	RefID                        sql.NullInt64
	Settled                      bool
}

// Poll reads everything above the cursor, records it (existing rows are left alone) and moves the
// cursor past the leading run of settled rows. It returns how many rows were new.
func (r *Reader) Poll(ctx context.Context) (int, error) {
	var low int64
	err := r.store.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO wms_bridge.read_cursor (org_id) VALUES (wms_bridge.current_org())
			ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT low_water FROM wms_bridge.read_cursor`).Scan(&low)
	})
	if err != nil {
		return 0, err
	}

	newRows, newLow, after, settledRun := 0, low, low, true
	for {
		rows, err := r.page(ctx, after)
		if err != nil {
			return newRows, fmt.Errorf("read wms ledger: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		n, err := r.record(ctx, rows)
		if err != nil {
			return newRows, err
		}
		newRows += n
		for _, row := range rows {
			if settledRun && row.Settled {
				newLow = row.ID
			} else {
				settledRun = false
			}
		}
		after = rows[len(rows)-1].ID
		if len(rows) < pageSize {
			break
		}
	}
	if newLow != low {
		err = r.store.Tx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE wms_bridge.read_cursor SET low_water = $1, updated_at = now()`, newLow)
			return err
		})
	}
	return newRows, err
}

func (r *Reader) page(ctx context.Context, after int64) ([]ledgerRow, error) {
	rows, err := r.wms.QueryContext(ctx, `
		SELECT l.id, l.movement_type, p.sku, p.title, coalesce(p.domain, ''), w.code, w.name,
			CAST(l.qty AS CHAR), l.uom, CAST(coalesce(p.buy_price, 0) AS CHAR),
			DATE_FORMAT(l.posted_at, '%Y-%m-%d %H:%i:%s'),
			coalesce(l.reference_type, ''), l.reference_id, coalesce(l.operator_name, ''), coalesce(l.notes, ''),
			DATE_FORMAT(l.created_at, '%Y-%m-%d %H:%i:%s')
		FROM stock_ledgers l
		JOIN products p ON p.id = l.product_id
		JOIN warehouses w ON w.id = l.warehouse_id
		WHERE l.id > ?
		ORDER BY l.id
		LIMIT ?`, after, pageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledgerRow
	cutoff := time.Now().Add(-settleAfter)
	for rows.Next() {
		var x ledgerRow
		var createdAt string
		if err := rows.Scan(&x.ID, &x.Movement, &x.SKU, &x.Title, &x.Domain, &x.WarehouseCode, &x.WarehouseName,
			&x.Qty, &x.UOM, &x.UnitCost, &x.PostedAt, &x.RefType, &x.RefID, &x.Operator, &x.Notes, &createdAt); err != nil {
			return nil, err
		}
		created, err := time.ParseInLocation(time.DateTime, createdAt, wmsLocation)
		if err != nil {
			return nil, fmt.Errorf("ledger %d created_at %q: %w", x.ID, createdAt, err)
		}
		x.Settled = created.Before(cutoff)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (r *Reader) record(ctx context.Context, rows []ledgerRow) (int, error) {
	n := 0
	err := r.store.Tx(ctx, func(tx pgx.Tx) error {
		for _, x := range rows {
			tag, err := tx.Exec(ctx, `
				INSERT INTO wms_bridge.ledger_event (org_id, ledger_id, movement_type, sku, domain, product_title,
					wms_warehouse, qty, uom, unit_cost, posted_at, reference_type, reference_id, operator, notes)
				VALUES (wms_bridge.current_org(), $1, $2, $3, $4, $5, $6, $7::numeric, $8, $9::numeric,
					$10::timestamp AT TIME ZONE $11, $12, $13, $14, $15)
				ON CONFLICT DO NOTHING`,
				x.ID, x.Movement, x.SKU, x.Domain, x.Title, x.WarehouseCode, x.Qty, x.UOM, x.UnitCost,
				x.PostedAt, wmsTimezone, x.RefType, x.RefID, x.Operator, x.Notes)
			if err != nil {
				return fmt.Errorf("record ledger %d: %w", x.ID, err)
			}
			if tag.RowsAffected() == 1 {
				n++
				if _, err := tx.Exec(ctx, `INSERT INTO wms_bridge.warehouse_map (org_id, wms_code, wms_name, erpnext_warehouse)
					VALUES (wms_bridge.current_org(), $1, $2, '') ON CONFLICT DO NOTHING`, x.WarehouseCode, x.WarehouseName); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return n, err
}
