package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"github.com/jackc/pgx/v5"
)

// API exposes the bridge state for operations: what was sent, what is stuck and why.
type API struct{ store *pgstore.Store }

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /summary", a.summary)
	mux.HandleFunc("GET /events", a.events)
	mux.HandleFunc("POST /events/{id}/retry", a.retry)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func internalError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "err", err)
	writeJSON(w, 500, map[string]string{"error": "internal error"})
}

func (a *API) summary(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		counts := map[string]int{"pending": 0, "sent": 0, "dead": 0}
		rows, err := tx.Query(r.Context(), `SELECT status, count(*) FROM wms_bridge.ledger_event GROUP BY status`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			var n int
			if err := rows.Scan(&s, &n); err != nil {
				return err
			}
			counts[s] = n
		}
		if err := rows.Err(); err != nil {
			return err
		}
		var low int64
		_ = tx.QueryRow(r.Context(), `SELECT low_water FROM wms_bridge.read_cursor`).Scan(&low)
		whRows, err := tx.Query(r.Context(), `SELECT wms_code, wms_name, erpnext_warehouse FROM wms_bridge.warehouse_map ORDER BY wms_code`)
		if err != nil {
			return err
		}
		whs, err := pgx.CollectRows(whRows, pgx.RowToMap)
		if err != nil {
			return err
		}
		out["events"], out["read_cursor"], out["warehouses"] = counts, low, whs
		return nil
	})
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT ledger_id, movement_type, sku, wms_warehouse, qty::text, uom, posted_at,
			status, attempts, last_error, coalesce(stock_entry, '') AS stock_entry
			FROM wms_bridge.ledger_event WHERE ($1 = '' OR status = $1) ORDER BY ledger_id DESC LIMIT 200`, r.URL.Query().Get("status"))
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToMap)
		return err
	})
	if err != nil {
		internalError(w, err)
		return
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

// retry puts a dead event back in the queue, after a person fixed the cause in ERPNext.
func (a *API) retry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "id must be a ledger id"})
		return
	}
	var n int64
	err = a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `UPDATE wms_bridge.ledger_event SET status = 'pending', attempts = 0, next_attempt_at = now()
			WHERE ledger_id = $1 AND status = 'dead'`, id)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		internalError(w, err)
		return
	}
	if n == 0 {
		writeJSON(w, 409, map[string]string{"error": "no dead event with this ledger id"})
		return
	}
	writeJSON(w, 200, map[string]any{"ledger_id": id, "status": "pending"})
}
