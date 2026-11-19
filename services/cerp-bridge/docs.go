package main

// Goods receipts, supplier invoices and payments: ConstructionERP records them, ERPNext books them.
//
//	receipt (confirmed)              -> Purchase Receipt against the booked ERPNext PO
//	invoice (payable; approved/sent/paid) -> Purchase Invoice (against the PO when there is one)
//	payment (on a payable invoice)   -> Payment Entry: cash to the supplier + Thai WHT as a deduction
//
// Each kind is read like purchase orders (updated_at cursor with look-back, content hash) into
// cerp_bridge.doc_state and posted with its own idempotency key. A document whose prerequisite is
// not in ERPNext yet (receipt before its PO, payment before its invoice) waits and retries.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/aideveloper-cloud/kgarden-platform/internal/erpnext"
	"github.com/jackc/pgx/v5"
)

// permanentError marks a document the bridge can never post as it stands; it goes dead at once.
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

// notYetError: the document is dated in the future (ERPNext refuses future-dated receipts and
// payments); it waits until that day without counting as a failed attempt.
type notYetError struct{ date string }

func (e *notYetError) Error() string {
	return "dated " + e.date + "; waits until that day to be booked"
}

func checkNotFuture(date string) error {
	if date > time.Now().In(bangkok).Format("2006-01-02") {
		return &notYetError{date}
	}
	return nil
}

var bangkok = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		return time.FixedZone("ICT", 7*3600)
	}
	return loc
}()

// ── Payloads ────────────────────────────────────────────────────────────────

type Receipt struct {
	ID            string        `json:"id"`
	POID          string        `json:"po_id"`
	Date          string        `json:"receipt_date"`
	DeliveryNote  string        `json:"delivery_note"`
	Notes         string        `json:"notes"`
	Items         []ReceiptItem `json:"items"`
	Status        string        `json:"-"`
	updatedAtRead time.Time
}

type ReceiptItem struct {
	POLine   int    `json:"po_line"` // 1-based position of the PO line (same order the PO was booked in)
	Received string `json:"received"`
	Rejected string `json:"rejected"`
	Reason   string `json:"reason"`
}

type Invoice struct {
	ID            string        `json:"id"`
	Number        string        `json:"invoice_number"`
	POID          string        `json:"po_id"`
	ProjectID     string        `json:"project_id"`
	ProjectName   string        `json:"project_name"`
	ProjectCode   string        `json:"project_code"`
	VendorID      string        `json:"vendor_id"`
	VendorName    string        `json:"vendor_name"`
	VendorTaxID   string        `json:"vendor_tax_id"`
	Date          string        `json:"invoice_date"`
	DueDate       string        `json:"due_date"`
	Currency      string        `json:"currency"`
	Subtotal      string        `json:"amount_subtotal"`
	Tax           string        `json:"tax_amount"`
	Retention     string        `json:"retention_amount"`
	Total         string        `json:"amount_total"`
	Items         []InvoiceItem `json:"items"`
	Status        string        `json:"-"`
	updatedAtRead time.Time
}

type InvoiceItem struct {
	Description  string `json:"description"`
	Qty          string `json:"qty"`
	Unit         string `json:"unit"`
	Rate         string `json:"rate"`
	Amount       string `json:"amount"`
	CostCategory string `json:"cost_category"`
}

type Payment struct {
	ID            string `json:"id"`
	InvoiceID     string `json:"invoice_id"`
	Date          string `json:"payment_date"`
	Amount        string `json:"amount"` // cash to the supplier
	Reference     string `json:"reference"`
	Refund        bool   `json:"is_refund"`
	WhtType       string `json:"wht_type"`
	WhtRate       string `json:"wht_rate"`
	WhtAmount     string `json:"wht_amount"`
	WhtCertNo     string `json:"wht_certificate_no"`
	updatedAtRead time.Time
}

func hashOf(v any) string {
	b, _ := json.Marshal(v)
	return (&PO{Number: string(b)}).hash()
}

// ── Reading ─────────────────────────────────────────────────────────────────

type docRow struct {
	kind, id, ref, status string
	live                  bool
	payload               any
	updatedAt             time.Time
}

// PollDocs reads changed receipts, payable invoices and their payments since each kind's cursor.
func (r *Reader) PollDocs(ctx context.Context) (int, error) {
	changed := 0
	for _, kind := range []string{"receipt", "invoice", "payment"} {
		var after time.Time
		err := r.store.Tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO cerp_bridge.kind_cursor (org_id, kind) VALUES (cerp_bridge.current_org(), $1)
				ON CONFLICT DO NOTHING`, kind); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT updated_after FROM cerp_bridge.kind_cursor WHERE kind = $1`, kind).Scan(&after)
		})
		if err != nil {
			return changed, err
		}
		var rows []docRow
		switch kind {
		case "receipt":
			rows, err = r.readReceipts(ctx, after.Add(-lookback))
		case "invoice":
			rows, err = r.readInvoices(ctx, after.Add(-lookback))
		case "payment":
			rows, err = r.readPayments(ctx, after.Add(-lookback))
		}
		if err != nil {
			return changed, fmt.Errorf("read %ss: %w", kind, err)
		}
		n, err := r.recordDocs(ctx, kind, rows, after)
		changed += n
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func (r *Reader) recordDocs(ctx context.Context, kind string, rows []docRow, after time.Time) (int, error) {
	changed := 0
	maxSeen := after
	err := r.store.Tx(ctx, func(tx pgx.Tx) error {
		for _, d := range rows {
			if d.updatedAt.After(maxSeen) {
				maxSeen = d.updatedAt
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO cerp_bridge.doc_state (org_id, kind, cerp_id, ref, cerp_status, live, content_hash, payload, cerp_updated_at)
				VALUES (cerp_bridge.current_org(), $1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (org_id, kind, cerp_id) DO UPDATE SET
					ref = EXCLUDED.ref, cerp_status = EXCLUDED.cerp_status, live = EXCLUDED.live, content_hash = EXCLUDED.content_hash,
					payload = EXCLUDED.payload, cerp_updated_at = EXCLUDED.cerp_updated_at,
					sync_status = 'pending', attempts = 0, next_attempt_at = now(), last_error = ''
				WHERE cerp_bridge.doc_state.live IS DISTINCT FROM EXCLUDED.live
					OR cerp_bridge.doc_state.content_hash IS DISTINCT FROM EXCLUDED.content_hash`,
				kind, d.id, d.ref, d.status, d.live, hashOf(d.payload), d.payload, d.updatedAt)
			if err != nil {
				return fmt.Errorf("record %s %s: %w", kind, d.ref, err)
			}
			if tag.RowsAffected() > 0 {
				changed++
			} else if _, err := tx.Exec(ctx, `UPDATE cerp_bridge.doc_state SET cerp_status = $3 WHERE kind = $1 AND cerp_id = $2`,
				kind, d.id, d.status); err != nil {
				return err
			}
		}
		if maxSeen.After(after) {
			_, err := tx.Exec(ctx, `UPDATE cerp_bridge.kind_cursor SET updated_after = $2 WHERE kind = $1`, kind, maxSeen)
			return err
		}
		return nil
	})
	return changed, err
}

func (r *Reader) readReceipts(ctx context.Context, since time.Time) ([]docRow, error) {
	rows, err := r.cerp.Query(ctx, `
		SELECT id::text, po_id::text, status, receipt_date, coalesce(delivery_note_number, ''), coalesce(notes, ''), updated_at
		FROM oe_procurement_goods_receipt WHERE updated_at > $1 ORDER BY updated_at LIMIT 500`, since)
	if err != nil {
		return nil, err
	}
	var grs []Receipt
	for rows.Next() {
		var g Receipt
		if err := rows.Scan(&g.ID, &g.POID, &g.Status, &g.Date, &g.DeliveryNote, &g.Notes, &g.updatedAtRead); err != nil {
			rows.Close()
			return nil, err
		}
		grs = append(grs, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []docRow
	for _, g := range grs {
		items, err := r.cerp.Query(ctx, `
			SELECT x.line, i.quantity_received, i.quantity_rejected, coalesce(i.rejection_reason, '')
			FROM oe_procurement_gr_item i
			JOIN (SELECT id, row_number() OVER (ORDER BY sort_order, id) AS line FROM oe_procurement_po_item WHERE po_id = $2) x
				ON x.id = i.po_item_id
			WHERE i.receipt_id = $1 ORDER BY x.line`, g.ID, g.POID)
		if err != nil {
			return nil, err
		}
		for items.Next() {
			var it ReceiptItem
			if err := items.Scan(&it.POLine, &it.Received, &it.Rejected, &it.Reason); err != nil {
				items.Close()
				return nil, err
			}
			g.Items = append(g.Items, it)
		}
		items.Close()
		if err := items.Err(); err != nil {
			return nil, err
		}
		ref := g.DeliveryNote
		if ref == "" {
			ref = "GR-" + g.ID[:8]
		}
		out = append(out, docRow{"receipt", g.ID, ref, g.Status, g.Status == "confirmed", g, g.updatedAtRead})
	}
	return out, nil
}

// Invoice statuses in which the supplier's bill is a liability ERPNext must carry.
var liveInvoiceStatuses = map[string]bool{"approved": true, "sent": true, "paid": true, "credit_note_issued": true}

func (r *Reader) readInvoices(ctx context.Context, since time.Time) ([]docRow, error) {
	rows, err := r.cerp.Query(ctx, `
		SELECT inv.id::text, inv.invoice_number, inv.status,
			coalesce(inv.purchase_order_id::text, inv.metadata->>'po_id', ''), inv.project_id::text,
			coalesce(pr.name, ''), coalesce(pr.project_code, ''),
			coalesce(inv.contact_id, ''), coalesce(nullif(c.legal_name, ''), c.company_name, ''), coalesce(c.vat_number, ''),
			inv.invoice_date, coalesce(inv.due_date, ''), coalesce(nullif(inv.currency_code, ''), 'THB'),
			inv.amount_subtotal::text, inv.tax_amount::text, inv.retention_amount::text, inv.amount_total::text, inv.updated_at
		FROM oe_finance_invoice inv
		LEFT JOIN oe_projects_project pr ON pr.id = inv.project_id
		LEFT JOIN oe_contacts_contact c ON c.id::text = inv.contact_id
		WHERE inv.invoice_direction = 'payable' AND inv.updated_at > $1
		ORDER BY inv.updated_at LIMIT 500`, since)
	if err != nil {
		return nil, err
	}
	var invs []Invoice
	for rows.Next() {
		var v Invoice
		if err := rows.Scan(&v.ID, &v.Number, &v.Status, &v.POID, &v.ProjectID, &v.ProjectName, &v.ProjectCode,
			&v.VendorID, &v.VendorName, &v.VendorTaxID, &v.Date, &v.DueDate, &v.Currency,
			&v.Subtotal, &v.Tax, &v.Retention, &v.Total, &v.updatedAtRead); err != nil {
			rows.Close()
			return nil, err
		}
		invs = append(invs, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []docRow
	for _, v := range invs {
		items, err := r.cerp.Query(ctx, `
			SELECT description, quantity::text, coalesce(unit, ''), unit_rate::text, amount::text, coalesce(cost_category, '')
			FROM oe_finance_invoice_item WHERE invoice_id = $1 ORDER BY sort_order, id`, v.ID)
		if err != nil {
			return nil, err
		}
		for items.Next() {
			var it InvoiceItem
			if err := items.Scan(&it.Description, &it.Qty, &it.Unit, &it.Rate, &it.Amount, &it.CostCategory); err != nil {
				items.Close()
				return nil, err
			}
			v.Items = append(v.Items, it)
		}
		items.Close()
		if err := items.Err(); err != nil {
			return nil, err
		}
		out = append(out, docRow{"invoice", v.ID, v.Number, v.Status, liveInvoiceStatuses[v.Status], v, v.updatedAtRead})
	}
	return out, nil
}

func (r *Reader) readPayments(ctx context.Context, since time.Time) ([]docRow, error) {
	rows, err := r.cerp.Query(ctx, `
		SELECT p.id::text, p.invoice_id::text, p.payment_date, p.amount::text, coalesce(p.reference, ''), p.is_refund,
			coalesce(p.wht_type, ''), coalesce(p.wht_rate, ''), coalesce(p.wht_amount, 0)::text, coalesce(p.wht_certificate_no, ''),
			inv.invoice_number, p.updated_at
		FROM oe_finance_payment p JOIN oe_finance_invoice inv ON inv.id = p.invoice_id
		WHERE inv.invoice_direction = 'payable' AND p.updated_at > $1
		ORDER BY p.updated_at LIMIT 500`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []docRow
	for rows.Next() {
		var p Payment
		var invNo string
		if err := rows.Scan(&p.ID, &p.InvoiceID, &p.Date, &p.Amount, &p.Reference, &p.Refund,
			&p.WhtType, &p.WhtRate, &p.WhtAmount, &p.WhtCertNo, &invNo, &p.updatedAtRead); err != nil {
			return nil, err
		}
		out = append(out, docRow{"payment", p.ID, invNo + " / " + p.Date, "recorded", true, p, p.updatedAtRead})
	}
	return out, rows.Err()
}

// ── Posting ─────────────────────────────────────────────────────────────────

type docState struct {
	Kind, ID, Ref, Status string
	Live                  bool
	Payload               []byte
	ERPNextDoc            string
	Attempts              int
}

var docDoctype = map[string]string{"receipt": "Purchase Receipt", "invoice": "Purchase Invoice", "payment": "Payment Entry"}

// DrainDocs posts due documents, receipts and invoices before payments.
func (p *Poster) DrainDocs(ctx context.Context) (int, error) {
	n := 0
	for {
		handled, err := p.processDoc(ctx)
		if err != nil || !handled {
			return n, err
		}
		n++
	}
}

func (p *Poster) processDoc(ctx context.Context) (bool, error) {
	handled := false
	err := p.store.Tx(ctx, func(tx pgx.Tx) error {
		var s docState
		err := tx.QueryRow(ctx, `
			SELECT kind, cerp_id::text, ref, cerp_status, live, payload, coalesce(erpnext_doc, ''), attempts
			FROM cerp_bridge.doc_state
			WHERE sync_status = 'pending' AND next_attempt_at <= now()
			ORDER BY CASE kind WHEN 'receipt' THEN 1 WHEN 'invoice' THEN 2 ELSE 3 END, cerp_updated_at
			LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&s.Kind, &s.ID, &s.Ref, &s.Status, &s.Live, &s.Payload, &s.ERPNextDoc, &s.Attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		handled = true

		doc, err := p.reconcileDoc(ctx, tx, &s)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE cerp_bridge.doc_state SET sync_status = 'synced', erpnext_doc = nullif($3, ''),
				attempts = 0, last_error = '', synced_at = now() WHERE kind = $1 AND cerp_id = $2`, s.Kind, s.ID, doc)
			return err
		}
		var later *notYetError
		if errors.As(err, &later) {
			_, uerr := tx.Exec(ctx, `UPDATE cerp_bridge.doc_state SET last_error = $3,
				next_attempt_at = ($4::date)::timestamp AT TIME ZONE 'Asia/Bangkok' WHERE kind = $1 AND cerp_id = $2`,
				s.Kind, s.ID, err.Error(), later.date)
			return uerr
		}
		attempts := s.Attempts + 1
		slog.Warn("constructionerp document failed", "kind", s.Kind, "ref", s.Ref, "attempt", attempts, "err", err)
		var perm *permanentError
		if errors.As(err, &perm) || (attempts >= p.cfg.MaxAttempts && !erpnext.IsNegativeStock(err)) {
			_, uerr := tx.Exec(ctx, `UPDATE cerp_bridge.doc_state SET sync_status = 'dead', attempts = $3, last_error = $4
				WHERE kind = $1 AND cerp_id = $2`, s.Kind, s.ID, attempts, err.Error())
			return uerr
		}
		backoff := int(math.Min(math.Pow(2, float64(attempts)), 300))
		_, uerr := tx.Exec(ctx, `UPDATE cerp_bridge.doc_state SET attempts = $3, last_error = $4, next_attempt_at = now() + make_interval(secs => $5)
			WHERE kind = $1 AND cerp_id = $2`, s.Kind, s.ID, attempts, err.Error(), backoff)
		return uerr
	})
	return handled, err
}

func (p *Poster) reconcileDoc(ctx context.Context, tx pgx.Tx, s *docState) (string, error) {
	doctype := docDoctype[s.Kind]
	key := fmt.Sprintf("cerp-%s-%s", s.Kind, s.ID)
	if !s.Live {
		if s.ERPNextDoc == "" {
			return "", nil
		}
		if err := p.cancelDoc(ctx, doctype, s.ERPNextDoc); err != nil {
			return s.ERPNextDoc, fmt.Errorf("cancel %s %s: %w", doctype, s.ERPNextDoc, err)
		}
		return "", auditDoc(ctx, tx, s, "cancelled", s.ERPNextDoc)
	}

	name, docstatus, err := p.erp.FindByField(ctx, doctype, "kg_idempotency_key", key)
	if err != nil {
		return "", err
	}
	switch {
	case name != "" && docstatus == 1:
		return name, nil
	case name != "" && docstatus == 2:
		return "", &permanentError{fmt.Sprintf("%s %s for %s was cancelled in ERPNext; needs a person to decide", doctype, name, s.Ref)}
	case name != "":
		// A draft left by an earlier attempt: drop it and build the document again, so a draft
		// with wrong figures is never submitted as it stands.
		if err := p.erp.Delete(ctx, doctype, name); err != nil {
			return "", fmt.Errorf("remove stale draft %s: %w", name, err)
		}
	}

	var build func(context.Context, pgx.Tx, *docState, string) (map[string]any, string, error)
	switch s.Kind {
	case "receipt":
		build = p.buildReceipt
	case "invoice":
		build = p.buildInvoice
	default:
		build = p.buildPayment
	}
	doc, wantTotal, err := build(ctx, tx, s, key)
	if err != nil {
		return "", err
	}
	created, err := p.erp.Insert(ctx, doctype, doc)
	if erpnext.IsDuplicate(err) {
		return "", errors.New("key conflict; will look it up again")
	}
	if err != nil {
		return "", fmt.Errorf("insert %s: %w", doctype, err)
	}
	name, _ = created["name"].(string)
	if wantTotal != "" {
		if err := sameTotal(created["grand_total"], wantTotal); err != nil {
			return "", fmt.Errorf("draft %s not submitted: %w", name, err)
		}
	}
	if _, err := p.erp.Submit(ctx, doctype, name); err != nil {
		return "", fmt.Errorf("submit %s: %w", name, err)
	}
	return name, auditDoc(ctx, tx, s, "booked", name)
}

func (p *Poster) cancelDoc(ctx context.Context, doctype, name string) error {
	doc, err := p.erp.Get(ctx, doctype, name)
	var ee *erpnext.Error
	if errors.As(err, &ee) && ee.Status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	if ds, _ := doc["docstatus"].(float64); ds == 1 {
		return p.erp.Cancel(ctx, doctype, name)
	}
	return nil
}

// bookedPO is the ERPNext Purchase Order currently standing for a ConstructionERP PO.
func bookedPO(ctx context.Context, tx pgx.Tx, cerpPOID string) (string, error) {
	var name string
	err := tx.QueryRow(ctx, `SELECT coalesce(erpnext_po, '') FROM cerp_bridge.po_state WHERE cerp_po_id::text = $1 AND sync_status = 'synced'`,
		cerpPOID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) || name == "" {
		return "", fmt.Errorf("purchase order %s is not booked in ERPNext yet", cerpPOID)
	}
	return name, err
}

func (p *Poster) buildReceipt(ctx context.Context, tx pgx.Tx, s *docState, key string) (map[string]any, string, error) {
	var g Receipt
	if err := json.Unmarshal(s.Payload, &g); err != nil {
		return nil, "", err
	}
	if err := checkNotFuture(g.Date); err != nil {
		return nil, "", err
	}
	erpPO, err := bookedPO(ctx, tx, g.POID)
	if err != nil {
		return nil, "", err
	}
	po, err := p.erp.Get(ctx, "Purchase Order", erpPO)
	if err != nil {
		return nil, "", err
	}
	// PO line position -> ERPNext PO row name (rows were booked in ConstructionERP's line order).
	rowByLine := map[int]string{}
	for _, raw := range asList(po["items"]) {
		row := raw.(map[string]any)
		rowByLine[int(num(row["idx"]))] = str(row["name"])
	}
	accepted := map[string]float64{}
	var notes []string
	for _, it := range g.Items {
		row, ok := rowByLine[it.POLine]
		if !ok {
			return nil, "", &permanentError{fmt.Sprintf("receipt line for PO line %d has no row in ERPNext PO %s", it.POLine, erpPO)}
		}
		qty := parseNum(it.Received) - parseNum(it.Rejected)
		if qty > 0 {
			accepted[row] += qty
		}
		if parseNum(it.Rejected) > 0 {
			notes = append(notes, fmt.Sprintf("line %d rejected %s: %s", it.POLine, it.Rejected, it.Reason))
		}
	}
	if len(accepted) == 0 {
		return nil, "", &permanentError{"receipt accepts nothing (all lines rejected or zero)"}
	}
	doc, err := p.erp.CallMethod(ctx, "erpnext.buying.doctype.purchase_order.mapper.make_purchase_receipt",
		map[string]any{"source_name": erpPO})
	if err != nil {
		return nil, "", fmt.Errorf("map receipt from %s: %w", erpPO, err)
	}
	var items []any
	for _, raw := range asList(doc["items"]) {
		row := raw.(map[string]any)
		if qty, ok := accepted[str(row["purchase_order_item"])]; ok {
			row["qty"], row["received_qty"], row["rejected_qty"] = qty, qty, 0
			items = append(items, row)
		}
	}
	if len(items) != len(accepted) {
		return nil, "", fmt.Errorf("ERPNext PO %s has nothing left to receive on some received lines", erpPO)
	}
	doc["items"] = items
	doc["posting_date"], doc["set_posting_time"] = g.Date, 1
	doc["supplier_delivery_note"] = g.DeliveryNote
	doc["kg_idempotency_key"] = key
	doc["remarks"] = strings.TrimSpace("ConstructionERP goods receipt " + s.Ref + "\n" + strings.Join(notes, "\n") + "\n" + g.Notes)
	return cleanDocMap(doc), "", nil
}

func (p *Poster) buildInvoice(ctx context.Context, tx pgx.Tx, s *docState, key string) (map[string]any, string, error) {
	var v Invoice
	if err := json.Unmarshal(s.Payload, &v); err != nil {
		return nil, "", err
	}
	if !isZero(v.Retention) {
		return nil, "", &permanentError{"invoices with retention are not booked yet"}
	}
	supplier, err := p.ensureSupplier(ctx, &PO{VendorID: v.VendorID, VendorName: v.VendorName, VendorTaxID: v.VendorTaxID})
	if err != nil {
		return nil, "", fmt.Errorf("supplier: %w", err)
	}
	project, err := p.ensureProject(ctx, &PO{ProjectID: v.ProjectID, ProjectName: v.ProjectName, ProjectCode: v.ProjectCode})
	if err != nil {
		return nil, "", fmt.Errorf("project: %w", err)
	}

	var doc map[string]any
	if v.POID != "" {
		erpPO, err := bookedPO(ctx, tx, v.POID)
		if err != nil {
			return nil, "", err
		}
		doc, err = p.erp.CallMethod(ctx, "erpnext.buying.doctype.purchase_order.mapper.make_purchase_invoice",
			map[string]any{"source_name": erpPO})
		if err != nil {
			return nil, "", fmt.Errorf("map invoice from %s: %w", erpPO, err)
		}
		// Bill each invoice line against the PO row with the same text and rate.
		pool := asList(doc["items"])
		used := make([]bool, len(pool))
		var items []any
		for _, line := range v.Items {
			matched := false
			for i, raw := range pool {
				row := raw.(map[string]any)
				if !used[i] && str(row["description"]) == line.Description && math.Abs(num(row["rate"])-parseNum(line.Rate)) < 0.005 {
					used[i], matched = true, true
					row["qty"] = json.Number(line.Qty)
					items = append(items, row)
					break
				}
			}
			if !matched {
				items = append(items, p.freeLine(line, project))
			}
		}
		doc["items"] = items
	} else {
		doc = map[string]any{"company": p.cfg.Company, "supplier": supplier, "currency": v.Currency}
		var items []any
		for _, line := range v.Items {
			items = append(items, p.freeLine(line, project))
		}
		doc["items"] = items
		if !isZero(v.Tax) {
			doc["taxes"] = []any{map[string]any{
				"charge_type": "Actual", "account_head": p.cfg.VATAccount, "tax_amount": json.Number(v.Tax),
				"description": "ภาษีมูลค่าเพิ่ม", "category": "Total", "add_deduct_tax": "Add",
			}}
		}
	}
	for _, line := range v.Items {
		if err := p.ensureUOM(ctx, unitOr(line.Unit)); err != nil {
			return nil, "", err
		}
	}
	doc["posting_date"], doc["set_posting_time"] = v.Date, 1
	doc["bill_no"], doc["bill_date"] = v.Number, v.Date
	if v.DueDate != "" && v.DueDate >= v.Date {
		doc["due_date"] = v.DueDate
	} else {
		delete(doc, "due_date")
		delete(doc, "payment_schedule")
	}
	doc["project"] = project
	doc["kg_idempotency_key"], doc["kg_cerp_invoice_number"] = key, v.Number
	doc["remarks"] = "ConstructionERP invoice " + v.Number
	return cleanDocMap(doc), v.Total, nil
}

func (p *Poster) freeLine(line InvoiceItem, project string) map[string]any {
	code, ok := costItems[strings.ToLower(strings.TrimSpace(line.CostCategory))]
	if !ok {
		code = "CON-OTHER"
	}
	return map[string]any{
		"item_code": code, "description": line.Description, "qty": json.Number(line.Qty), "uom": unitOr(line.Unit),
		"stock_uom": "Nos", "conversion_factor": 1, "rate": json.Number(line.Rate), "project": project,
	}
}

func (p *Poster) buildPayment(ctx context.Context, tx pgx.Tx, s *docState, key string) (map[string]any, string, error) {
	var pay Payment
	if err := json.Unmarshal(s.Payload, &pay); err != nil {
		return nil, "", err
	}
	if pay.Refund {
		return nil, "", &permanentError{"supplier refunds are not booked yet"}
	}
	if err := checkNotFuture(pay.Date); err != nil {
		return nil, "", err
	}
	var pi string
	err := tx.QueryRow(ctx, `SELECT coalesce(erpnext_doc, '') FROM cerp_bridge.doc_state
		WHERE kind = 'invoice' AND cerp_id::text = $1 AND sync_status = 'synced'`, pay.InvoiceID).Scan(&pi)
	if errors.Is(err, pgx.ErrNoRows) || pi == "" {
		return nil, "", fmt.Errorf("invoice %s is not booked in ERPNext yet", pay.InvoiceID)
	}
	if err != nil {
		return nil, "", err
	}
	doc, err := p.erp.CallMethod(ctx, "erpnext.accounts.doctype.payment_entry.payment_entry.get_payment_entry",
		map[string]any{"dt": "Purchase Invoice", "dn": pi})
	if err != nil {
		return nil, "", fmt.Errorf("map payment from %s: %w", pi, err)
	}
	cash, wht := parseNum(pay.Amount), parseNum(pay.WhtAmount)
	refs := asList(doc["references"])
	if len(refs) == 0 {
		return nil, "", fmt.Errorf("purchase invoice %s has nothing outstanding", pi)
	}
	ref := refs[0].(map[string]any)
	ref["allocated_amount"] = round2(cash + wht)
	doc["references"] = []any{ref}
	doc["paid_amount"], doc["received_amount"] = round2(cash), round2(cash)
	doc["source_exchange_rate"], doc["target_exchange_rate"] = 1, 1
	doc["posting_date"], doc["reference_date"] = pay.Date, pay.Date
	refNo := pay.Reference
	if refNo == "" {
		refNo = "CERP " + pay.ID[:8]
	}
	doc["reference_no"] = refNo
	doc["kg_idempotency_key"] = key
	var deductions []any
	if wht > 0 {
		costCenter, err := p.companyCostCenter(ctx)
		if err != nil {
			return nil, "", err
		}
		// On a Pay entry a deduction is posted with the opposite sign: -WHT credits the WHT
		// payable account, so Creditors is debited the full invoice and Cash credited the cash.
		deductions = append(deductions, map[string]any{
			"account": p.cfg.WHTAccount, "cost_center": costCenter, "amount": -round2(wht),
			"description": fmt.Sprintf("หัก ณ ที่จ่าย %s%% (%s) %s", pay.WhtRate, pay.WhtType, pay.WhtCertNo),
		})
		doc["kg_wht_type"], doc["kg_wht_certificate_no"] = pay.WhtType, pay.WhtCertNo
		if r, err := strconv.ParseFloat(pay.WhtRate, 64); err == nil {
			doc["kg_wht_rate"] = r
		}
	}
	doc["deductions"] = deductions
	doc["remarks"] = "ConstructionERP payment " + s.Ref
	return cleanDocMap(doc), "", nil
}

func (p *Poster) companyCostCenter(ctx context.Context) (string, error) {
	c, err := p.erp.Get(ctx, "Company", p.cfg.Company)
	if err != nil {
		return "", err
	}
	if cc := str(c["cost_center"]); cc != "" {
		return cc, nil
	}
	return "", fmt.Errorf("company %s has no default cost center", p.cfg.Company)
}

func auditDoc(ctx context.Context, tx pgx.Tx, s *docState, action, erpDoc string) error {
	_, err := tx.Exec(ctx, `INSERT INTO cerp_bridge.audit_log (org_id, kind, cerp_id, action, erpnext_po, detail)
		VALUES (cerp_bridge.current_org(), $1, $2, $3, $4, $5)`, s.Kind, s.ID, s.Kind+"."+action, erpDoc, s.Ref)
	return err
}

// ── small helpers ───────────────────────────────────────────────────────────

func asList(v any) []any { l, _ := v.([]any); return l }

func str(v any) string { s, _ := v.(string); return s }

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func parseNum(s string) float64 { f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return f }

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func unitOr(u string) string {
	if u = strings.TrimSpace(u); u == "" {
		return "Nos"
	}
	return u
}

func cleanDocMap(doc map[string]any) map[string]any { return erpnext.CleanDoc(doc).(map[string]any) }
