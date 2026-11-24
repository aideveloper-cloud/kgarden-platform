package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/aideveloper-cloud/kgarden-platform/internal/erpnext"
	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"github.com/jackc/pgx/v5"
)

// Config says where WMS stock lands in ERPNext.
type Config struct {
	Company          string // ERPNext company, e.g. "K Garden"
	ParentWarehouse  string // group warehouse new WMS warehouses are created under
	TransitWarehouse string // where stock sits between TRANSFER_OUT and TRANSFER_IN
	WIPWarehouse     string // raw material cut or consumed for production goes here; Work Orders backflush from it
	FGStaging        string // legacy (separate MES): finished goods waited here until the FG POS received them; leave empty
	CreateMasters    bool   // create missing Items, UOMs and Warehouses in ERPNext
	MaxAttempts      int
}

// WMS base UOM -> ERPNext UOM. Anything else is title-cased (SHEET -> Sheet) and created if missing.
var uomMap = map[string]string{"KG": "Kg", "PCS": "Nos", "PC": "Nos", "EA": "Nos"}

// Poster sends recorded ledger events to ERPNext, one Stock Entry per ledger row.
type Poster struct {
	store *pgstore.Store
	erp   *erpnext.Client
	cfg   Config

	mu    sync.Mutex
	known map[string]bool // masters already confirmed in ERPNext ("Item:RM-PIPE", "UOM:Sheet")
}

type event struct {
	LedgerID                                   int64
	Movement, SKU, Domain, Title, WarehouseWMS string
	Qty, UOM, UnitCost, RefType, Operator      string
	Notes, WorkOrder                           string
	RefID                                      *int64
	PostedAt                                   time.Time
	Attempts                                   int
	Warehouse                                  string // mapped ERPNext warehouse ('' = not yet mapped)
	WarehouseName                              string
}

// Drain posts due events in ledger order until none are left.
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
		var ev event
		err := tx.QueryRow(ctx, `
			SELECT e.ledger_id, e.movement_type, e.sku, e.domain, e.product_title, e.wms_warehouse, e.qty::text, e.uom,
				e.unit_cost::text, e.reference_type, e.operator, e.notes, e.work_order, e.reference_id, e.posted_at, e.attempts,
				coalesce(m.erpnext_warehouse, ''), coalesce(m.wms_name, e.wms_warehouse)
			FROM wms_bridge.ledger_event e
			LEFT JOIN wms_bridge.warehouse_map m ON m.org_id = e.org_id AND m.wms_code = e.wms_warehouse
			WHERE e.status = 'pending' AND e.next_attempt_at <= now()
			ORDER BY e.ledger_id LIMIT 1 FOR UPDATE OF e SKIP LOCKED`).Scan(
			&ev.LedgerID, &ev.Movement, &ev.SKU, &ev.Domain, &ev.Title, &ev.WarehouseWMS, &ev.Qty, &ev.UOM,
			&ev.UnitCost, &ev.RefType, &ev.Operator, &ev.Notes, &ev.WorkOrder, &ev.RefID, &ev.PostedAt, &ev.Attempts,
			&ev.Warehouse, &ev.WarehouseName)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		handled = true

		name, err := p.post(ctx, tx, &ev)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE wms_bridge.ledger_event SET status = 'sent', stock_entry = $2, attempts = attempts + 1,
				last_error = '', sent_at = now() WHERE ledger_id = $1`, ev.LedgerID, name)
			return err
		}

		attempts := ev.Attempts + 1
		slog.Warn("wms ledger event failed", "ledger_id", ev.LedgerID, "attempt", attempts, "err", err)
		// Not enough stock yet (e.g. output reported before the RM POS cut the materials into WIP):
		// wait for the other side instead of giving up.
		if attempts >= p.cfg.MaxAttempts && !erpnext.IsNegativeStock(err) {
			_, uerr := tx.Exec(ctx, `UPDATE wms_bridge.ledger_event SET status = 'dead', attempts = $2, last_error = $3
				WHERE ledger_id = $1`, ev.LedgerID, attempts, err.Error())
			return uerr
		}
		backoff := int(math.Min(math.Pow(2, float64(attempts)), 300))
		_, uerr := tx.Exec(ctx, `UPDATE wms_bridge.ledger_event SET attempts = $2, last_error = $3,
			next_attempt_at = now() + make_interval(secs => $4) WHERE ledger_id = $1`, ev.LedgerID, attempts, err.Error(), backoff)
		return uerr
	})
	return handled, err
}

// post finds or creates the submitted Stock Entry for one ledger row and returns its name.
func (p *Poster) post(ctx context.Context, tx pgx.Tx, ev *event) (string, error) {
	key := fmt.Sprintf("wms-ledger-%d", ev.LedgerID)
	for attempt := 0; attempt < 2; attempt++ {
		existing, err := p.erp.FindStockEntryByKey(ctx, key)
		if err != nil {
			return "", fmt.Errorf("lookup by key: %w", err)
		}
		if existing != nil {
			switch existing.DocStatus {
			case 1:
				return existing.Name, nil
			case 0:
				_, err := p.erp.Submit(ctx, "Stock Entry", existing.Name)
				return existing.Name, err
			default:
				return "", fmt.Errorf("stock entry %s for ledger %d was cancelled in ERPNext; needs a person to decide", existing.Name, ev.LedgerID)
			}
		}

		if err := p.ensureMasters(ctx, tx, ev); err != nil {
			return "", err
		}
		var doc map[string]any
		if ev.isProductionOutput() {
			doc, err = p.buildManufacture(ctx, ev, key)
		} else {
			doc, err = p.buildEntry(ev, key)
		}
		if err != nil {
			return "", err
		}
		created, err := p.erp.InsertSubmitted(ctx, "Stock Entry", doc)
		if erpnext.IsDuplicate(err) {
			continue // an earlier attempt got there first; look it up again
		}
		if err != nil {
			return "", fmt.Errorf("insert stock entry: %w", err)
		}
		name, _ := created["name"].(string)
		return name, nil
	}
	return "", errors.New("stock entry key conflict did not resolve")
}

// Production flow rules (2026-11-18: WMS moves raw material to WIP, Work Orders backflush from it;
// 2026-11-24: the shop floor is the WMS production POS, whose output is received straight into a
// WMS warehouse and posted here as a Manufacture entry).
// The WMS marks each posting with the flow that made it (reference_type).
var (
	// Raw material leaving a warehouse for production: the RM tablet "cut" and Spoolman filament use.
	productionIssueRefs = map[string]bool{"warehouse_floor_cut": true, "spoolman_consumption_event": true}
	// Finished goods arriving from production: the FG tablet "receive".
	productionReceiptRefs = map[string]bool{"warehouse_floor_receive": true}
)

func (p *Poster) toWIP(ev *event, inbound bool) bool {
	return p.cfg.WIPWarehouse != "" && !inbound && ev.Domain != "FG" &&
		(productionIssueRefs[ev.RefType] || ev.Movement == "CONSUMPTION")
}

func (p *Poster) fromFGStaging(ev *event, inbound bool) bool {
	return p.cfg.FGStaging != "" && inbound && ev.Domain == "FG" && productionReceiptRefs[ev.RefType]
}

func (p *Poster) buildEntry(ev *event, key string) (map[string]any, error) {
	qty := strings.TrimLeft(ev.Qty, "-")
	inbound := !strings.HasPrefix(ev.Qty, "-")
	item := map[string]any{
		"item_code":         ev.SKU,
		"qty":               jsonNum(qty),
		"uom":               erpUOM(ev.UOM),
		"conversion_factor": 1,
	}
	var purpose string
	switch {
	case p.toWIP(ev, inbound):
		// Raw material leaves the store for production: it is not consumed yet, the MES
		// backflushes it from WIP against the BOM when output is reported.
		purpose = "Material Transfer"
		item["s_warehouse"], item["t_warehouse"] = ev.Warehouse, p.cfg.WIPWarehouse
	case p.fromFGStaging(ev, inbound):
		// Finished goods arriving from production: the MES Manufacture entry already brought them
		// into stock (in the staging warehouse), so this is a move, not new stock.
		purpose = "Material Transfer"
		item["s_warehouse"], item["t_warehouse"] = p.cfg.FGStaging, ev.Warehouse
	case ev.Movement == "TRANSFER_OUT":
		purpose = "Material Transfer"
		item["s_warehouse"], item["t_warehouse"] = ev.Warehouse, p.cfg.TransitWarehouse
	case ev.Movement == "TRANSFER_IN":
		purpose = "Material Transfer"
		item["s_warehouse"], item["t_warehouse"] = p.cfg.TransitWarehouse, ev.Warehouse
	case inbound:
		purpose = "Material Receipt"
		item["t_warehouse"] = ev.Warehouse
		if isZero(ev.UnitCost) {
			item["allow_zero_valuation_rate"] = 1 // WMS has no cost for this product yet
		} else {
			item["basic_rate"] = jsonNum(ev.UnitCost)
		}
	default:
		purpose = "Material Issue"
		item["s_warehouse"] = ev.Warehouse
	}

	local := ev.PostedAt.In(wmsLocation)
	ref := ev.RefType
	if ev.RefID != nil {
		ref = fmt.Sprintf("%s #%d", ev.RefType, *ev.RefID)
	}
	remarks := fmt.Sprintf("WMS ledger #%d · %s · %s · %s", ev.LedgerID, ev.Movement, ev.WarehouseName, ref)
	if ev.Operator != "" {
		remarks += " · ผู้ทำรายการ: " + ev.Operator
	}
	if ev.Notes != "" {
		remarks += " · " + ev.Notes
	}
	return map[string]any{
		"stock_entry_type":   purpose,
		"purpose":            purpose,
		"company":            p.cfg.Company,
		"set_posting_time":   1,
		"posting_date":       local.Format("2006-01-02"),
		"posting_time":       local.Format("15:04:05"),
		"kg_idempotency_key": key,
		"remarks":            remarks,
		"items":              []any{item},
	}, nil
}

// productionOutputRef marks finished goods reported at the WMS production POS.
const productionOutputRef = "production_output"

func (ev *event) isProductionOutput() bool {
	return ev.RefType == productionOutputRef && ev.WorkOrder != "" && !strings.HasPrefix(ev.Qty, "-")
}

// buildManufacture asks ERPNext for the Manufacture entry of this Work Order and quantity (BOM
// materials backflushed from WIP, as the kgarden Work Order rule sets) and sends the finished goods
// to the WMS warehouse they were received into instead of the Work Order's own FG warehouse.
func (p *Poster) buildManufacture(ctx context.Context, ev *event, key string) (map[string]any, error) {
	doc, err := p.erp.MakeManufactureEntry(ctx, ev.WorkOrder, ev.Qty)
	if err != nil {
		return nil, fmt.Errorf("manufacture entry for %s: %w", ev.WorkOrder, err)
	}
	items, _ := doc["items"].([]any)
	finished := 0
	for _, raw := range items {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fin, _ := row["is_finished_item"].(float64); fin == 1 || (row["item_code"] == ev.SKU && row["t_warehouse"] != nil && row["s_warehouse"] == nil) {
			row["t_warehouse"] = ev.Warehouse
			finished++
		}
		// Materials the WMS received without a cost carry a zero rate into WIP (see buildEntry);
		// ERPNext would refuse to consume them, so the same allowance follows them here. Consumed
		// rows keep their real valuation; the flag only stops the error when it is zero. Never on
		// the finished good: there it would book the output at zero instead of the material cost.
		if src, _ := row["s_warehouse"].(string); src != "" && row["t_warehouse"] == nil {
			row["allow_zero_valuation_rate"] = 1
		}
	}
	if finished == 0 {
		return nil, fmt.Errorf("manufacture entry for %s has no finished item row for %s", ev.WorkOrder, ev.SKU)
	}
	local := ev.PostedAt.In(wmsLocation)
	remarks := fmt.Sprintf("WMS ledger #%d · ผลิตเสร็จตาม %s · เข้าคลัง %s · รายงานเมื่อ %s",
		ev.LedgerID, ev.WorkOrder, ev.WarehouseName, local.Format("2006-01-02 15:04"))
	if ev.Operator != "" {
		remarks += " · ผู้ทำรายการ: " + ev.Operator
	}
	// Booked when it was reported, unless an earlier try failed (typically: its materials were not
	// in WIP yet). Then it is booked now: a backdated entry would still find WIP short at the report
	// time, because the cut that brought the materials is dated later, and would never go through.
	if ev.Attempts == 0 {
		doc["set_posting_time"] = 1
		doc["posting_date"] = local.Format("2006-01-02")
		doc["posting_time"] = local.Format("15:04:05")
	} else {
		doc["set_posting_time"] = 0
		delete(doc, "posting_date")
		delete(doc, "posting_time")
	}
	doc["kg_idempotency_key"] = key
	doc["remarks"] = remarks
	return doc, nil
}

// ensureMasters makes sure the warehouse, UOM and item an event needs exist in ERPNext.
func (p *Poster) ensureMasters(ctx context.Context, tx pgx.Tx, ev *event) error {
	if ev.Warehouse == "" {
		wh, err := p.ensureWarehouse(ctx, ev.WarehouseName)
		if err != nil {
			return fmt.Errorf("warehouse %s: %w", ev.WarehouseWMS, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE wms_bridge.warehouse_map SET erpnext_warehouse = $2 WHERE wms_code = $1`, ev.WarehouseWMS, wh); err != nil {
			return err
		}
		ev.Warehouse = wh
	}
	uom := erpUOM(ev.UOM)
	if err := p.ensure(ctx, "UOM", uom, func() map[string]any {
		return map[string]any{"uom_name": uom}
	}); err != nil {
		return err
	}
	return p.ensure(ctx, "Item", ev.SKU, func() map[string]any {
		group := "Raw Material"
		if ev.Domain == "FG" {
			group = "Products"
		}
		return map[string]any{
			"item_code": ev.SKU, "item_name": truncate(ev.Title, 140), "item_group": group, "stock_uom": uom,
			"is_stock_item": 1, "include_item_in_manufacturing": 1,
			"description": "สร้างอัตโนมัติจาก WMS (production-pos)",
		}
	})
}

func (p *Poster) ensure(ctx context.Context, doctype, name string, build func() map[string]any) error {
	cacheKey := doctype + ":" + name
	p.mu.Lock()
	ok := p.known[cacheKey]
	p.mu.Unlock()
	if ok {
		return nil
	}
	exists, err := p.erp.Exists(ctx, doctype, name)
	if err != nil {
		return fmt.Errorf("check %s %s: %w", doctype, name, err)
	}
	if !exists {
		if !p.cfg.CreateMasters {
			return fmt.Errorf("%s %s does not exist in ERPNext (create it there, or set WMS_BRIDGE_CREATE_MASTERS=1)", doctype, name)
		}
		if _, err := p.erp.Insert(ctx, doctype, build()); err != nil && !erpnext.IsDuplicate(err) {
			return fmt.Errorf("create %s %s: %w", doctype, name, err)
		}
		slog.Info("created in erpnext", "doctype", doctype, "name", name)
	}
	p.mu.Lock()
	p.known[cacheKey] = true
	p.mu.Unlock()
	return nil
}

// ensureWarehouse finds the ERPNext warehouse with this warehouse_name in the company, or creates it.
func (p *Poster) ensureWarehouse(ctx context.Context, wmsName string) (string, error) {
	found, err := p.erp.List(ctx, "Warehouse", []string{"name"},
		[][]any{{"warehouse_name", "=", wmsName}, {"company", "=", p.cfg.Company}}, 1)
	if err != nil {
		return "", err
	}
	if len(found) > 0 {
		return found[0]["name"].(string), nil
	}
	if !p.cfg.CreateMasters {
		return "", fmt.Errorf("no ERPNext warehouse named %q", wmsName)
	}
	created, err := p.erp.Insert(ctx, "Warehouse", map[string]any{
		"warehouse_name": wmsName, "parent_warehouse": p.cfg.ParentWarehouse, "company": p.cfg.Company,
	})
	if err != nil {
		return "", err
	}
	slog.Info("created in erpnext", "doctype", "Warehouse", "name", created["name"])
	return created["name"].(string), nil
}

func erpUOM(wms string) string {
	if u, ok := uomMap[strings.ToUpper(wms)]; ok {
		return u
	}
	s := strings.ToLower(wms)
	if s == "" {
		return "Nos"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func isZero(n string) bool {
	return strings.Trim(strings.TrimLeft(n, "-"), "0.") == ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
