package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type API struct {
	store *pgstore.Store
	sync  *Syncer
	auth  *Auth
}

// Routes: everything but /healthz and sign-in needs a session from ERPNext-managed access.
// Operators and supervisors work the floor; only supervisors pull Work Orders and read the outbox.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	a.auth.Routes(mux)
	floor := []string{"operator", "supervisor"}
	mux.HandleFunc("POST /sync/work-orders", a.auth.Require(a.syncWorkOrders, "supervisor"))
	mux.HandleFunc("GET /work-orders", a.auth.Require(a.listWorkOrders, floor...))
	mux.HandleFunc("GET /work-orders/{name}", a.auth.Require(a.getWorkOrder, floor...))
	mux.HandleFunc("POST /work-orders/{name}/start", a.auth.Require(a.startOperation, floor...))
	mux.HandleFunc("POST /work-orders/{name}/stop", a.auth.Require(a.stopOperation, floor...))
	mux.HandleFunc("POST /work-orders/{name}/output", a.auth.Require(a.reportOutput, floor...))
	mux.HandleFunc("GET /outbox", a.auth.Require(a.listOutbox, "supervisor"))
	return mux
}

// operatorOf is who did it: the signed-in person, never a name typed into the request.
func operatorOf(r *http.Request) string {
	if s := sessionFrom(r); s != nil {
		return s.Label()
	}
	return ""
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func fail(status int, format string, args ...any) error {
	return &httpError{status, fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeJSON(w, he.status, map[string]string{"error": he.msg})
		return
	}
	slog.Error("request failed", "err", err)
	writeJSON(w, 500, map[string]string{"error": "internal error"})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fail(400, "invalid JSON body: %v", err)
	}
	return nil
}

func (a *API) syncWorkOrders(w http.ResponseWriter, r *http.Request) {
	n, err := a.sync.Run(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]int{"synced": n})
}

func (a *API) listWorkOrders(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT m.name, m.production_item, m.item_name, m.qty::text, m.produced_qty::text,
				coalesce((SELECT sum(good_qty) FROM mes.production_output p
					WHERE p.work_order = m.name AND p.status = 'pending'), 0)::text,
				m.stock_uom, m.status,
				EXISTS (SELECT 1 FROM mes.operation_log l WHERE l.work_order = m.name AND l.ended_at IS NULL)
			FROM mes.mirror_work_order m
			WHERE m.status IN ('Not Started', 'In Process')
			ORDER BY m.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name, item, itemName, qty, produced, pending, uom, status string
			var running bool
			if err := rows.Scan(&name, &item, &itemName, &qty, &produced, &pending, &uom, &status, &running); err != nil {
				return err
			}
			out = append(out, map[string]any{
				"name": name, "production_item": item, "item_name": itemName, "qty": qty,
				"produced_qty": produced, "pending_qty": pending, "stock_uom": uom, "status": status, "running": running,
			})
		}
		return rows.Err()
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (a *API) getWorkOrder(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var out map[string]any
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		var item, itemName, qty, produced, uom, status string
		err := tx.QueryRow(r.Context(), `SELECT production_item, item_name, qty::text, produced_qty::text, stock_uom, status
			FROM mes.mirror_work_order WHERE name = $1`, name).Scan(&item, &itemName, &qty, &produced, &uom, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail(404, "work order %s not found (run POST /sync/work-orders)", name)
		}
		if err != nil {
			return err
		}
		// waiting_reason: why a pending output has not reached ERPNext yet (last outbox error).
		outputs, err := queryMaps(r, tx, `SELECT o.id::text, o.good_qty::text, o.scrap_qty::text, o.operator, o.status,
			coalesce(o.stock_entry, '') AS stock_entry, o.created_at,
			CASE WHEN o.status = 'pending' THEN coalesce(e.last_error, '') ELSE '' END AS waiting_reason
			FROM mes.production_output o
			LEFT JOIN mes.outbox_event e ON e.aggregate_id = o.id AND e.event_type = 'erpnext.manufacture'
			WHERE o.work_order = $1 ORDER BY o.created_at`, name)
		if err != nil {
			return err
		}
		logs, err := queryMaps(r, tx, `SELECT id::text, operator, started_at, ended_at FROM mes.operation_log
			WHERE work_order = $1 ORDER BY started_at`, name)
		if err != nil {
			return err
		}
		out = map[string]any{"name": name, "production_item": item, "item_name": itemName, "qty": qty,
			"produced_qty": produced, "stock_uom": uom, "status": status, "outputs": outputs, "operation_logs": logs}
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}

type operatorReq struct {
	Operator string `json:"operator"`
	Note     string `json:"note"`
}

func (a *API) startOperation(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req operatorReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	req.Operator = operatorOf(r)
	var id string
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		if _, err := lockOpenWorkOrder(r, tx, name); err != nil {
			return err
		}
		err := tx.QueryRow(r.Context(), `INSERT INTO mes.operation_log (org_id, work_order, operator, note)
			VALUES (mes.current_org(), $1, $2, $3) RETURNING id::text`, name, req.Operator, req.Note).Scan(&id)
		if isUniqueViolation(err) {
			return fail(409, "work order %s is already running", name)
		}
		if err != nil {
			return err
		}
		return audit(r.Context(), tx, req.Operator, "operation.start", "work_order", name, map[string]string{"operation_log": id})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"operation_log": id})
}

func (a *API) stopOperation(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req operatorReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	req.Operator = operatorOf(r)
	var id string
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(), `UPDATE mes.operation_log SET ended_at = now(), note = CASE WHEN $2 = '' THEN note ELSE $2 END
			WHERE work_order = $1 AND ended_at IS NULL RETURNING id::text`, name, req.Note).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail(409, "work order %s is not running", name)
		}
		if err != nil {
			return err
		}
		return audit(r.Context(), tx, req.Operator, "operation.stop", "work_order", name, map[string]string{"operation_log": id})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"operation_log": id})
}

type outputReq struct {
	GoodQty     json.Number `json:"good_qty"`
	ScrapQty    json.Number `json:"scrap_qty"`
	ScrapReason string      `json:"scrap_reason"`
	Operator    string      `json:"operator"`
}

// reportOutput records finished quantity from the floor. The client must send an Idempotency-Key
// header; a retry with the same key returns the original result instead of counting twice.
// Good quantity is queued in the outbox and posted to ERPNext as a Manufacture Stock Entry.
// Scrap is recorded locally only for now (posting it to ERPNext is a later step).
func (a *API) reportOutput(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeErr(w, fail(400, "Idempotency-Key header is required"))
		return
	}
	var req outputReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.GoodQty == "" {
		req.GoodQty = "0"
	}
	if req.ScrapQty == "" {
		req.ScrapQty = "0"
	}
	req.Operator = operatorOf(r)

	var outputID, status string
	replayed := false
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		// Replay: same key already recorded.
		err := tx.QueryRow(r.Context(), `SELECT id::text, status FROM mes.production_output WHERE client_key = $1 AND work_order = $2`,
			key, name).Scan(&outputID, &status)
		if err == nil {
			replayed = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		if _, err := lockOpenWorkOrder(r, tx, name); err != nil {
			return err
		}
		var positive, scrapPositive, fits bool
		err = tx.QueryRow(r.Context(), `
			SELECT $1::numeric > 0, $2::numeric > 0,
				$1::numeric <= m.qty - m.produced_qty - coalesce((SELECT sum(good_qty) FROM mes.production_output p
					WHERE p.work_order = m.name AND p.status = 'pending'), 0)
			FROM mes.mirror_work_order m WHERE m.name = $3`, string(req.GoodQty), string(req.ScrapQty), name).Scan(&positive, &scrapPositive, &fits)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
				return fail(400, "good_qty and scrap_qty must be numbers")
			}
			return err
		}
		if !positive && !scrapPositive {
			return fail(400, "good_qty or scrap_qty must be greater than 0")
		}
		if !fits {
			return fail(409, "good_qty %s is more than the remaining quantity of %s", req.GoodQty, name)
		}
		if scrapPositive && strings.TrimSpace(req.ScrapReason) == "" {
			return fail(400, "scrap_reason is required when scrap_qty > 0")
		}

		status = "pending"
		if !positive {
			status = "posted" // nothing to send to ERPNext
		}
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO mes.production_output (org_id, work_order, good_qty, scrap_qty, operator, client_key, status)
			VALUES (mes.current_org(), $1, $2::numeric, $3::numeric, $4, $5, $6) RETURNING id::text`,
			name, string(req.GoodQty), string(req.ScrapQty), req.Operator, key, status).Scan(&outputID); err != nil {
			return err
		}
		if scrapPositive {
			if _, err := tx.Exec(r.Context(), `INSERT INTO mes.scrap_log (org_id, output_id, qty, reason)
				VALUES (mes.current_org(), $1, $2::numeric, $3)`, outputID, string(req.ScrapQty), req.ScrapReason); err != nil {
				return err
			}
		}
		if positive {
			payload := map[string]string{"work_order": name, "qty": string(req.GoodQty), "output_id": outputID}
			if _, err := tx.Exec(r.Context(), `
				INSERT INTO mes.outbox_event (org_id, event_type, aggregate_id, idempotency_key, payload)
				VALUES (mes.current_org(), 'erpnext.manufacture', $1, $2, $3)`,
				outputID, "mes-output-"+outputID, payload); err != nil {
				return err
			}
		}
		return audit(r.Context(), tx, req.Operator, "output.report", "work_order", name, map[string]string{
			"output_id": outputID, "good_qty": string(req.GoodQty), "scrap_qty": string(req.ScrapQty)})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	code := 201
	if replayed {
		code = 200
	}
	writeJSON(w, code, map[string]any{"output_id": outputID, "status": status, "replayed": replayed})
}

func (a *API) listOutbox(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = queryMaps(r, tx, `SELECT id::text, event_type, idempotency_key, status, attempts, last_error,
			payload, result, next_attempt_at FROM mes.outbox_event
			WHERE ($1 = '' OR status = $1) ORDER BY created_at DESC LIMIT 100`, r.URL.Query().Get("status"))
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

// lockOpenWorkOrder locks the mirrored Work Order row and checks the floor may still work on it.
func lockOpenWorkOrder(r *http.Request, tx pgx.Tx, name string) (string, error) {
	var status string
	err := tx.QueryRow(r.Context(), `SELECT status FROM mes.mirror_work_order WHERE name = $1 FOR UPDATE`, name).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fail(404, "work order %s not found (run POST /sync/work-orders)", name)
	}
	if err != nil {
		return "", err
	}
	if status != "Not Started" && status != "In Process" {
		return "", fail(409, "work order %s is %s", name, status)
	}
	return status, nil
}

func queryMaps(r *http.Request, tx pgx.Tx, sql string, args ...any) ([]map[string]any, error) {
	rows, err := tx.Query(r.Context(), sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToMap)
	if out == nil {
		out = []map[string]any{}
	}
	return out, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
