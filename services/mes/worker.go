package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aideveloper-cloud/kgarden-platform/internal/erpnext"
	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// Syncer mirrors submitted ERPNext Work Orders into mes.mirror_work_order (M5).
// ERPNext is the owner, so its values always overwrite ours.
type Syncer struct {
	store *pgstore.Store
	erp   *erpnext.Client
}

func (s *Syncer) Run(ctx context.Context) (int, error) {
	wos, err := s.erp.ListWorkOrders(ctx)
	if err != nil {
		return 0, fmt.Errorf("list work orders: %w", err)
	}
	err = s.store.Tx(ctx, func(tx pgx.Tx) error {
		for _, wo := range wos {
			if _, err := tx.Exec(ctx, `
				INSERT INTO mes.mirror_work_order (org_id, name, production_item, item_name, bom_no, qty, produced_qty,
					stock_uom, status, erpnext_modified, synced_at)
				VALUES (mes.current_org(), $1, $2, $3, $4, $5::numeric, $6::numeric, $7, $8, $9::timestamp AT TIME ZONE 'Asia/Bangkok', now())
				ON CONFLICT (org_id, name) DO UPDATE SET
					production_item = EXCLUDED.production_item, item_name = EXCLUDED.item_name, bom_no = EXCLUDED.bom_no,
					qty = EXCLUDED.qty, produced_qty = EXCLUDED.produced_qty, stock_uom = EXCLUDED.stock_uom,
					status = EXCLUDED.status, erpnext_modified = EXCLUDED.erpnext_modified, synced_at = now()`,
				wo.Name, wo.ProductionItem, wo.ItemName, wo.BOMNo, string(wo.Qty), numOr0(wo.ProducedQty),
				wo.StockUOM, wo.Status, wo.Modified); err != nil {
				return fmt.Errorf("upsert %s: %w", wo.Name, err)
			}
		}
		return nil
	})
	return len(wos), err
}

func numOr0(n json.Number) string {
	if n == "" {
		return "0"
	}
	return string(n)
}

// Worker drains the outbox: each pending event is locked (SKIP LOCKED, so several workers are safe),
// sent to ERPNext, and marked sent, retried with backoff, or moved to dead after maxAttempts.
type Worker struct {
	store       *pgstore.Store
	erp         *erpnext.Client
	sync        *Syncer
	maxAttempts int
}

func (w *Worker) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := w.Drain(ctx); err != nil {
			slog.Error("outbox drain", "err", err)
		} else if n > 0 {
			if _, err := w.sync.Run(ctx); err != nil {
				slog.Warn("re-sync after posting", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Drain processes due events one at a time until none are left, returning how many it handled.
func (w *Worker) Drain(ctx context.Context) (int, error) {
	n := 0
	for {
		handled, err := w.processOne(ctx)
		if err != nil || !handled {
			return n, err
		}
		n++
	}
}

type outboxEvent struct {
	ID, Type, Key string
	Attempts      int
	Payload       map[string]string
}

func (w *Worker) processOne(ctx context.Context) (bool, error) {
	handled := false
	err := w.store.Tx(ctx, func(tx pgx.Tx) error {
		var ev outboxEvent
		err := tx.QueryRow(ctx, `SELECT id::text, event_type, idempotency_key, attempts, payload FROM mes.outbox_event
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&ev.ID, &ev.Type, &ev.Key, &ev.Attempts, &ev.Payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		handled = true

		var result map[string]any
		switch ev.Type {
		case "erpnext.manufacture":
			result, err = w.postManufacture(ctx, tx, ev)
		default:
			err = fmt.Errorf("unknown event type %q", ev.Type)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE mes.outbox_event SET status = 'sent', attempts = attempts + 1, last_error = '', result = $2
				WHERE id = $1`, ev.ID, result)
			return err
		}

		attempts := ev.Attempts + 1
		slog.Warn("outbox event failed", "id", ev.ID, "type", ev.Type, "attempt", attempts, "err", err)
		// Raw material not in WIP yet (the WMS has not moved it): wait for it instead of failing the output.
		if attempts >= w.maxAttempts && !erpnext.IsNegativeStock(err) {
			if _, uerr := tx.Exec(ctx, `UPDATE mes.outbox_event SET status = 'dead', attempts = $2, last_error = $3 WHERE id = $1`,
				ev.ID, attempts, err.Error()); uerr != nil {
				return uerr
			}
			_, uerr := tx.Exec(ctx, `UPDATE mes.production_output SET status = 'failed' WHERE id = $1::uuid`, ev.Payload["output_id"])
			return uerr
		}
		backoff := time.Duration(math.Min(math.Pow(2, float64(attempts)), 300)) * time.Second
		_, uerr := tx.Exec(ctx, `UPDATE mes.outbox_event SET attempts = $2, last_error = $3, next_attempt_at = now() + $4::interval
			WHERE id = $1`, ev.ID, attempts, err.Error(), fmt.Sprintf("%d seconds", int(backoff.Seconds())))
		return uerr
	})
	return handled, err
}

// postManufacture creates (or finds) the submitted Manufacture Stock Entry for one reported output.
// The idempotency key is stored on the Stock Entry under a unique index, so a retry after a timeout
// finds the entry it already created instead of producing the goods twice.
func (w *Worker) postManufacture(ctx context.Context, tx pgx.Tx, ev outboxEvent) (map[string]any, error) {
	entry, err := w.findOrCreateManufacture(ctx, ev)
	if err != nil {
		return nil, err
	}
	name, _ := entry["name"].(string)
	outputID := ev.Payload["output_id"]

	if _, err := tx.Exec(ctx, `UPDATE mes.production_output SET status = 'posted', stock_entry = $2 WHERE id = $1::uuid`, outputID, name); err != nil {
		return nil, err
	}
	// Upsert: a resent event (crash after ERPNext accepted the entry) copies the same rows again,
	// and ERPNext's quantities win.
	items, _ := entry["items"].([]any)
	for _, raw := range items {
		it, _ := raw.(map[string]any)
		src, _ := it["s_warehouse"].(string)
		if src == "" {
			continue // the finished good row, not a consumed material
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mes.material_consumption (org_id, output_id, item_code, qty, stock_uom, source_warehouse)
			VALUES (mes.current_org(), $1::uuid, $2, $3::numeric, $4, $5)
			ON CONFLICT (output_id, item_code, source_warehouse) DO UPDATE SET qty = EXCLUDED.qty, stock_uom = EXCLUDED.stock_uom`,
			outputID, it["item_code"], fmt.Sprint(it["transfer_qty"]), it["stock_uom"], src); err != nil {
			return nil, err
		}
	}
	if err := audit(ctx, tx, "mes-worker", "output.posted", "stock_entry", name, map[string]string{"output_id": outputID, "work_order": ev.Payload["work_order"]}); err != nil {
		return nil, err
	}
	return map[string]any{"stock_entry": name}, nil
}

func (w *Worker) findOrCreateManufacture(ctx context.Context, ev outboxEvent) (map[string]any, error) {
	for attempt := 0; attempt < 2; attempt++ {
		existing, err := w.erp.FindStockEntryByKey(ctx, ev.Key)
		if err != nil {
			return nil, fmt.Errorf("lookup by key: %w", err)
		}
		if existing != nil {
			switch existing.DocStatus {
			case 1:
				return w.erp.Get(ctx, "Stock Entry", existing.Name)
			case 0:
				return w.erp.Submit(ctx, "Stock Entry", existing.Name)
			default:
				return nil, fmt.Errorf("stock entry %s for this output was cancelled in ERPNext; needs a person to decide", existing.Name)
			}
		}

		doc, err := w.erp.MakeManufactureEntry(ctx, ev.Payload["work_order"], ev.Payload["qty"])
		if err != nil {
			return nil, fmt.Errorf("make stock entry: %w", err)
		}
		doc["kg_idempotency_key"] = ev.Key
		doc["remarks"] = "Posted by K Garden MES, output " + ev.Payload["output_id"]
		created, err := w.erp.InsertSubmitted(ctx, "Stock Entry", doc)
		var ee *erpnext.Error
		if errors.As(err, &ee) && (ee.ExcType == "UniqueValidationError" || ee.ExcType == "DuplicateEntryError") {
			continue // a concurrent or earlier attempt won; look it up again
		}
		if err != nil {
			return nil, fmt.Errorf("insert stock entry: %w", err)
		}
		return created, nil
	}
	return nil, errors.New("stock entry key conflict did not resolve")
}
