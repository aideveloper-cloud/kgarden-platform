package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"github.com/jackc/pgx/v5"
)

// API shows what the bridge holds: each ConstructionERP PO, its ERPNext PO and any error.
type API struct{ store *pgstore.Store }

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /purchase-orders", a.list)
	mux.HandleFunc("POST /purchase-orders/{id}/retry", a.retry)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT cerp_po_id::text AS id, po_number, cerp_status, live, content_hash,
			coalesce(erpnext_po, '') AS erpnext_po, sync_status, attempts, last_error, payload->>'amount_total' AS amount_total, synced_at
			FROM cerp_bridge.po_state WHERE ($1 = '' OR sync_status = $1) ORDER BY cerp_updated_at DESC LIMIT 200`, r.URL.Query().Get("status"))
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToMap)
		return err
	})
	if err != nil {
		slog.Error("list", "err", err)
		writeJSON(w, 500, map[string]string{"error": "internal error"})
		return
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

// retry puts a dead PO back in the queue after a person fixed the cause.
func (a *API) retry(w http.ResponseWriter, r *http.Request) {
	var n int64
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `UPDATE cerp_bridge.po_state SET sync_status = 'pending', attempts = 0, next_attempt_at = now()
			WHERE cerp_po_id::text = $1 AND sync_status = 'dead'`, r.PathValue("id"))
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		slog.Error("retry", "err", err)
		writeJSON(w, 500, map[string]string{"error": "internal error"})
		return
	}
	if n == 0 {
		writeJSON(w, 409, map[string]string{"error": "no dead purchase order with this id"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "pending"})
}
