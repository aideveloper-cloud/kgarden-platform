package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aideveloper-cloud/kgarden-platform/internal/erpnext"
	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"github.com/jackc/pgx/v5"
)

type Config struct {
	Company       string // ERPNext company the construction business books in
	VATAccount    string // input VAT account of that company
	WHTAccount    string // Thai WHT deducted from supplier payments, owed to the Revenue Department
	SupplierGroup string
	MaxAttempts   int
}

// ConstructionERP cost category -> non-stock ERPNext item (created by kgarden.construction.setup).
var costItems = map[string]string{
	"material": "CON-MATERIAL", "materials": "CON-MATERIAL",
	"labor": "CON-LABOR", "labour": "CON-LABOR",
	"subcontract": "CON-SUBCONTRACT", "subcontractor": "CON-SUBCONTRACT",
	"equipment": "CON-EQUIPMENT", "plant": "CON-EQUIPMENT",
}

// Poster makes ERPNext match each pending PO version: a live PO has exactly one submitted ERPNext
// Purchase Order with the same content, a void one has none.
type Poster struct {
	store *pgstore.Store
	erp   *erpnext.Client
	cfg   Config

	mu    sync.Mutex
	known map[string]bool // "UOM:ถุง" already confirmed
}

type state struct {
	ID, Hash, Status string
	Live             bool
	Payload          PO
	ERPNextPO, Key   string
	Attempts         int
}

func (p *Poster) Drain(ctx context.Context) (int, error) {
	n := 0
	for {
		handled, err := p.processOne(ctx)
		if err != nil || !handled {
			return n, err
		}
		n++
	}
}

func (p *Poster) processOne(ctx context.Context) (bool, error) {
	handled := false
	err := p.store.Tx(ctx, func(tx pgx.Tx) error {
		var s state
		var payload []byte
		err := tx.QueryRow(ctx, `
			SELECT cerp_po_id::text, content_hash, cerp_status, live, payload, coalesce(erpnext_po, ''), coalesce(erpnext_key, ''), attempts
			FROM cerp_bridge.po_state
			WHERE sync_status = 'pending' AND next_attempt_at <= now()
			ORDER BY cerp_updated_at LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(
			&s.ID, &s.Hash, &s.Status, &s.Live, &payload, &s.ERPNextPO, &s.Key, &s.Attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		handled = true
		if err := json.Unmarshal(payload, &s.Payload); err != nil {
			return err
		}

		erpPO, key, err := p.reconcile(ctx, tx, &s)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE cerp_bridge.po_state SET sync_status = 'synced', erpnext_po = nullif($2, ''),
				erpnext_key = nullif($3, ''), attempts = 0, last_error = '', synced_at = now() WHERE cerp_po_id = $1`, s.ID, erpPO, key)
			return err
		}
		attempts := s.Attempts + 1
		slog.Warn("constructionerp po failed", "po", s.Payload.Number, "attempt", attempts, "err", err)
		if attempts >= p.cfg.MaxAttempts {
			_, uerr := tx.Exec(ctx, `UPDATE cerp_bridge.po_state SET sync_status = 'dead', attempts = $2, last_error = $3 WHERE cerp_po_id = $1`,
				s.ID, attempts, err.Error())
			return uerr
		}
		backoff := int(math.Min(math.Pow(2, float64(attempts)), 300))
		_, uerr := tx.Exec(ctx, `UPDATE cerp_bridge.po_state SET attempts = $2, last_error = $3, next_attempt_at = now() + make_interval(secs => $4)
			WHERE cerp_po_id = $1`, s.ID, attempts, err.Error(), backoff)
		return uerr
	})
	return handled, err
}

// reconcile returns the ERPNext PO (and its key) that now stands for this ConstructionERP PO.
func (p *Poster) reconcile(ctx context.Context, tx pgx.Tx, s *state) (string, string, error) {
	want := ""
	if s.Live {
		want = fmt.Sprintf("cerp-po-%s-%s", s.ID, s.Hash)
	}
	// An older version (edited after approval, or now draft/cancelled) is cancelled first.
	if s.ERPNextPO != "" && s.Key != want {
		if err := p.cancelPO(ctx, s.ERPNextPO); err != nil {
			return s.ERPNextPO, s.Key, fmt.Errorf("cancel %s: %w", s.ERPNextPO, err)
		}
		if err := audit(ctx, tx, s.ID, "po.cancelled", s.ERPNextPO, "ConstructionERP status "+s.Status); err != nil {
			return "", "", err
		}
	}
	if !s.Live {
		return "", "", nil
	}
	name, err := p.ensurePO(ctx, &s.Payload, want)
	if err != nil {
		return "", "", err
	}
	if name != s.ERPNextPO {
		if err := audit(ctx, tx, s.ID, "po.booked", name, s.Payload.Number+" total "+s.Payload.Total); err != nil {
			return "", "", err
		}
	}
	return name, want, nil
}

func (p *Poster) cancelPO(ctx context.Context, name string) error {
	doc, err := p.erp.Get(ctx, "Purchase Order", name)
	var ee *erpnext.Error
	if errors.As(err, &ee) && ee.Status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	switch ds, _ := doc["docstatus"].(float64); ds {
	case 1:
		return p.erp.Cancel(ctx, "Purchase Order", name)
	default:
		return nil // a draft never counted; a cancelled one is already gone
	}
}

// ensurePO finds the ERPNext PO for this version by key, or creates it; it is submitted only when
// ERPNext's grand total equals ConstructionERP's total, so a mapping mistake never books wrong money.
func (p *Poster) ensurePO(ctx context.Context, po *PO, key string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		name, docstatus, err := p.erp.FindByField(ctx, "Purchase Order", "kg_idempotency_key", key)
		if err != nil {
			return "", err
		}
		if name == "" {
			doc, err := p.buildPO(ctx, po, key)
			if err != nil {
				return "", err
			}
			created, err := p.erp.Insert(ctx, "Purchase Order", doc)
			if erpnext.IsDuplicate(err) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("insert purchase order: %w", err)
			}
			name, docstatus = created["name"].(string), 0
		}
		switch docstatus {
		case 1:
			return name, nil
		case 2:
			return "", fmt.Errorf("ERPNext PO %s for this version was cancelled by hand; needs a person to decide", name)
		}
		draft, err := p.erp.Get(ctx, "Purchase Order", name)
		if err != nil {
			return "", err
		}
		if err := sameTotal(draft["grand_total"], po.Total); err != nil {
			return "", fmt.Errorf("draft %s not submitted: %w", name, err)
		}
		if _, err := p.erp.Submit(ctx, "Purchase Order", name); err != nil {
			return "", fmt.Errorf("submit %s: %w", name, err)
		}
		return name, nil
	}
	return "", errors.New("purchase order key conflict did not resolve")
}

func (p *Poster) buildPO(ctx context.Context, po *PO, key string) (map[string]any, error) {
	supplier, err := p.ensureSupplier(ctx, po)
	if err != nil {
		return nil, fmt.Errorf("supplier: %w", err)
	}
	project, err := p.ensureProject(ctx, po)
	if err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	date := po.IssueDate
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	schedule := po.DeliveryDate
	if schedule == "" || schedule < date {
		schedule = date
	}

	var items []any
	for i, it := range po.Items {
		uom := strings.TrimSpace(it.Unit)
		if uom == "" {
			uom = "Nos"
		}
		if err := p.ensureUOM(ctx, uom); err != nil {
			return nil, err
		}
		code, ok := costItems[strings.ToLower(strings.TrimSpace(it.CostCategory))]
		if !ok {
			code = "CON-OTHER"
		}
		items = append(items, map[string]any{
			"item_code": code, "description": it.Description, "qty": json.Number(it.Qty), "uom": uom,
			"stock_uom": "Nos", "conversion_factor": 1, "rate": json.Number(it.Rate),
			"schedule_date": schedule, "project": project, "idx": i + 1,
		})
	}

	var taxes []any
	switch po.VatMode {
	case "exclusive", "inclusive":
		taxes = append(taxes, map[string]any{
			"charge_type": "On Net Total", "account_head": p.cfg.VATAccount, "rate": json.Number(orZero(po.VatRate)),
			"description": "ภาษีมูลค่าเพิ่ม " + po.VatRate + "%", "included_in_print_rate": boolInt(po.VatMode == "inclusive"),
			"category": "Total", "add_deduct_tax": "Add",
		})
	case "", "none":
	default:
		return nil, fmt.Errorf("unknown vat_mode %q", po.VatMode)
	}

	terms := "ConstructionERP " + po.Number
	if po.PaymentTerms != "" {
		terms += "\nเงื่อนไขการชำระ: " + po.PaymentTerms
	}
	if po.WhtType != "" && !isZero(po.WhtAmount) {
		// Booked on the purchase invoice / payment, not on the PO; recorded here so AP sees it.
		terms += fmt.Sprintf("\nหัก ณ ที่จ่าย %s%% (%s) = %s บาท", po.WhtRate, po.WhtType, po.WhtAmount)
	}
	return map[string]any{
		"company": p.cfg.Company, "supplier": supplier, "transaction_date": date, "schedule_date": schedule,
		"currency": po.Currency, "project": project, "kg_idempotency_key": key, "kg_cerp_po_number": po.Number,
		"terms": terms, "items": items, "taxes": taxes,
	}, nil
}

// ensureSupplier finds the supplier made from this ConstructionERP contact, adopts an existing one
// with the same tax ID, or creates it.
func (p *Poster) ensureSupplier(ctx context.Context, po *PO) (string, error) {
	if po.VendorID == "" {
		return "", errors.New("PO has no vendor")
	}
	name, _, err := p.erp.FindByField(ctx, "Supplier", "kg_cerp_id", po.VendorID)
	if err != nil || name != "" {
		return name, err
	}
	if po.VendorTaxID != "" {
		name, _, err = p.erp.FindByField(ctx, "Supplier", "tax_id", po.VendorTaxID)
		if err != nil {
			return "", err
		}
		if name != "" {
			_, err = p.erp.Update(ctx, "Supplier", name, map[string]any{"kg_cerp_id": po.VendorID})
			return name, err
		}
	}
	created, err := p.erp.Insert(ctx, "Supplier", map[string]any{
		"supplier_name": po.VendorName, "supplier_group": p.cfg.SupplierGroup, "supplier_type": "Company",
		"country": "Thailand", "tax_id": po.VendorTaxID, "kg_cerp_id": po.VendorID,
	})
	if erpnext.IsDuplicate(err) {
		name, _, err = p.erp.FindByField(ctx, "Supplier", "kg_cerp_id", po.VendorID)
		return name, err
	}
	if err != nil {
		return "", err
	}
	return created["name"].(string), nil
}

func (p *Poster) ensureProject(ctx context.Context, po *PO) (string, error) {
	name, _, err := p.erp.FindByField(ctx, "Project", "kg_cerp_id", po.ProjectID)
	if err != nil || name != "" {
		return name, err
	}
	title := strings.TrimSpace(strings.TrimSpace(po.ProjectCode) + " " + po.ProjectName)
	created, err := p.erp.Insert(ctx, "Project", map[string]any{
		"project_name": title, "company": p.cfg.Company, "kg_cerp_id": po.ProjectID, "status": "Open",
	})
	if erpnext.IsDuplicate(err) {
		name, _, err = p.erp.FindByField(ctx, "Project", "kg_cerp_id", po.ProjectID)
		return name, err
	}
	if err != nil {
		return "", err
	}
	return created["name"].(string), nil
}

func (p *Poster) ensureUOM(ctx context.Context, uom string) error {
	p.mu.Lock()
	ok := p.known[uom]
	p.mu.Unlock()
	if ok {
		return nil
	}
	exists, err := p.erp.Exists(ctx, "UOM", uom)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := p.erp.Insert(ctx, "UOM", map[string]any{"uom_name": uom}); err != nil && !erpnext.IsDuplicate(err) {
			return fmt.Errorf("create UOM %s: %w", uom, err)
		}
	}
	p.mu.Lock()
	p.known[uom] = true
	p.mu.Unlock()
	return nil
}

func audit(ctx context.Context, tx pgx.Tx, poID, action, erpPO, detail string) error {
	_, err := tx.Exec(ctx, `INSERT INTO cerp_bridge.audit_log (org_id, cerp_po_id, action, erpnext_po, detail)
		VALUES (cerp_bridge.current_org(), $1, $2, $3, $4)`, poID, action, erpPO, detail)
	return err
}

func sameTotal(erpTotal any, cerpTotal string) error {
	got, _ := erpTotal.(float64)
	want, err := strconv.ParseFloat(cerpTotal, 64)
	if err != nil {
		return fmt.Errorf("ConstructionERP total %q is not a number", cerpTotal)
	}
	if math.Abs(got-want) > 0.005 {
		return fmt.Errorf("ERPNext grand total %.2f differs from ConstructionERP total %.2f", got, want)
	}
	return nil
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

func isZero(s string) bool { return strings.Trim(strings.TrimLeft(s, "-"), "0.") == "" }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
