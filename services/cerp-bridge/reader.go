package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lookback re-reads POs changed shortly before the cursor: a transaction that committed late can
// carry an updated_at older than rows already seen. Re-reading is harmless (the hash is unchanged).
const lookback = 5 * time.Minute

// Statuses in which a ConstructionERP PO is a commitment ERPNext must carry. Draft (also an
// approved PO sent back for edits) and cancelled ones must have no ERPNext PO.
var liveStatuses = map[string]bool{"approved": true, "issued": true, "partially_received": true, "completed": true}

// PO is the part of a ConstructionERP purchase order that ERPNext books. It is also the payload
// stored per version; its hash decides whether ERPNext must get a new PO.
type PO struct {
	ID           string   `json:"id"`
	Number       string   `json:"po_number"`
	Status       string   `json:"-"` // not part of the content: issued/received keep the same ERPNext PO
	ProjectID    string   `json:"project_id"`
	ProjectName  string   `json:"project_name"`
	ProjectCode  string   `json:"project_code"`
	VendorID     string   `json:"vendor_id"`
	VendorName   string   `json:"vendor_name"`
	VendorTaxID  string   `json:"vendor_tax_id"`
	Currency     string   `json:"currency"`
	IssueDate    string   `json:"issue_date"`
	DeliveryDate string   `json:"delivery_date"`
	Subtotal     string   `json:"amount_subtotal"`
	Tax          string   `json:"tax_amount"`
	Total        string   `json:"amount_total"`
	VatMode      string   `json:"vat_mode"`
	VatRate      string   `json:"vat_rate"`
	WhtType      string   `json:"wht_type"`
	WhtRate      string   `json:"wht_rate"`
	WhtAmount    string   `json:"wht_amount"`
	PaymentTerms string   `json:"payment_terms"`
	Items        []POItem `json:"items"`
}

type POItem struct {
	Description  string `json:"description"`
	Qty          string `json:"qty"`
	Unit         string `json:"unit"`
	Rate         string `json:"rate"`
	Amount       string `json:"amount"`
	CostCategory string `json:"cost_category"`
	WhtApplies   bool   `json:"wht_applies"`
}

func (p *PO) hash() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// Reader polls ConstructionERP (read-only role) for purchase orders changed since the cursor.
type Reader struct {
	cerp  *pgxpool.Pool
	store *pgstore.Store
}

// Poll records every changed PO version and returns how many POs need work in ERPNext.
func (r *Reader) Poll(ctx context.Context) (int, error) {
	var after time.Time
	err := r.store.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO cerp_bridge.read_cursor (org_id) VALUES (cerp_bridge.current_org()) ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT updated_after FROM cerp_bridge.read_cursor`).Scan(&after)
	})
	if err != nil {
		return 0, err
	}

	pos, maxSeen, err := r.read(ctx, after.Add(-lookback))
	if err != nil {
		return 0, fmt.Errorf("read constructionerp: %w", err)
	}
	changed := 0
	err = r.store.Tx(ctx, func(tx pgx.Tx) error {
		for _, p := range pos {
			live, h := liveStatuses[p.Status], p.hash()
			// Only a new version (content or live/void changed) puts the PO back in the queue.
			tag, err := tx.Exec(ctx, `
				INSERT INTO cerp_bridge.po_state (org_id, cerp_po_id, po_number, cerp_status, live, content_hash, payload, cerp_updated_at)
				VALUES (cerp_bridge.current_org(), $1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (org_id, cerp_po_id) DO UPDATE SET
					po_number = EXCLUDED.po_number, cerp_status = EXCLUDED.cerp_status, live = EXCLUDED.live,
					content_hash = EXCLUDED.content_hash, payload = EXCLUDED.payload, cerp_updated_at = EXCLUDED.cerp_updated_at,
					sync_status = 'pending', attempts = 0, next_attempt_at = now(), last_error = ''
				WHERE cerp_bridge.po_state.live IS DISTINCT FROM EXCLUDED.live
					OR cerp_bridge.po_state.content_hash IS DISTINCT FROM EXCLUDED.content_hash`,
				p.ID, p.Number, p.Status, live, h, p, p.updatedAt)
			if err != nil {
				return fmt.Errorf("record po %s: %w", p.Number, err)
			}
			if tag.RowsAffected() > 0 {
				changed++
			} else if _, err := tx.Exec(ctx, `UPDATE cerp_bridge.po_state SET cerp_status = $2 WHERE cerp_po_id = $1`, p.ID, p.Status); err != nil {
				return err
			}
		}
		if maxSeen.After(after) {
			_, err := tx.Exec(ctx, `UPDATE cerp_bridge.read_cursor SET updated_after = $1`, maxSeen)
			return err
		}
		return nil
	})
	return changed, err
}

type readPO struct {
	PO
	updatedAt time.Time
}

func (r *Reader) read(ctx context.Context, since time.Time) ([]readPO, time.Time, error) {
	maxSeen := since
	rows, err := r.cerp.Query(ctx, `
		SELECT po.id::text, po.po_number, po.status, po.project_id::text, coalesce(pr.name, ''), coalesce(pr.project_code, ''),
			coalesce(po.vendor_contact_id, ''), coalesce(nullif(c.legal_name, ''), c.company_name, ''), coalesce(c.vat_number, ''),
			coalesce(nullif(po.currency_code, ''), nullif(pr.currency, ''), 'THB'), coalesce(po.issue_date, ''), coalesce(po.delivery_date, ''),
			po.amount_subtotal, po.tax_amount, po.amount_total, coalesce(po.vat_mode, ''), coalesce(po.vat_rate, ''),
			coalesce(po.wht_type, ''), coalesce(po.wht_rate, ''), coalesce(po.wht_amount, ''), coalesce(po.payment_terms, ''),
			po.updated_at
		FROM oe_procurement_po po
		LEFT JOIN oe_projects_project pr ON pr.id = po.project_id
		LEFT JOIN oe_contacts_contact c ON c.id::text = po.vendor_contact_id
		WHERE po.updated_at > $1
		ORDER BY po.updated_at
		LIMIT 500`, since)
	if err != nil {
		return nil, maxSeen, err
	}
	var out []readPO
	for rows.Next() {
		var p readPO
		if err := rows.Scan(&p.ID, &p.Number, &p.Status, &p.ProjectID, &p.ProjectName, &p.ProjectCode,
			&p.VendorID, &p.VendorName, &p.VendorTaxID, &p.Currency, &p.IssueDate, &p.DeliveryDate,
			&p.Subtotal, &p.Tax, &p.Total, &p.VatMode, &p.VatRate, &p.WhtType, &p.WhtRate, &p.WhtAmount, &p.PaymentTerms,
			&p.updatedAt); err != nil {
			rows.Close()
			return nil, maxSeen, err
		}
		if p.updatedAt.After(maxSeen) {
			maxSeen = p.updatedAt
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, maxSeen, err
	}
	for i := range out {
		items, err := r.cerp.Query(ctx, `
			SELECT description, quantity, coalesce(unit, ''), unit_rate, amount, coalesce(cost_category, ''), coalesce(wht_applies, false)
			FROM oe_procurement_po_item WHERE po_id = $1 ORDER BY sort_order, id`, out[i].ID)
		if err != nil {
			return nil, maxSeen, err
		}
		for items.Next() {
			var it POItem
			if err := items.Scan(&it.Description, &it.Qty, &it.Unit, &it.Rate, &it.Amount, &it.CostCategory, &it.WhtApplies); err != nil {
				items.Close()
				return nil, maxSeen, err
			}
			out[i].Items = append(out[i].Items, it)
		}
		items.Close()
		if err := items.Err(); err != nil {
			return nil, maxSeen, err
		}
	}
	return out, maxSeen, nil
}
