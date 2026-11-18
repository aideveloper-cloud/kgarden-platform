package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SyncProjects copies every ConstructionERP project into ERPNext (company KGF Construction), not
// only those that already have a purchase order, so a K Garden admin can grant access to any
// project (KG App CERP, scope Project) from the User form in ERPNext.
func (p *Poster) SyncProjects(ctx context.Context, cerp *pgxpool.Pool) (int, error) {
	var after time.Time
	err := p.store.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO cerp_bridge.kind_cursor (org_id, kind) VALUES (cerp_bridge.current_org(), 'project')
			ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT updated_after FROM cerp_bridge.kind_cursor WHERE kind = 'project'`).Scan(&after)
	})
	if err != nil {
		return 0, err
	}
	rows, err := cerp.Query(ctx, `SELECT id::text, name, coalesce(project_code, ''), updated_at FROM oe_projects_project
		WHERE updated_at > $1 ORDER BY updated_at LIMIT 200`, after.Add(-lookback))
	if err != nil {
		return 0, fmt.Errorf("read projects: %w", err)
	}
	type proj struct {
		id, name, code string
		at             time.Time
	}
	var list []proj
	for rows.Next() {
		var x proj
		if err := rows.Scan(&x.id, &x.name, &x.code, &x.at); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n, latest := 0, after
	for _, x := range list {
		if _, err := p.ensureProject(ctx, &PO{ProjectID: x.id, ProjectName: x.name, ProjectCode: x.code}); err != nil {
			return n, fmt.Errorf("project %s: %w", x.name, err)
		}
		n++
		if x.at.After(latest) {
			latest = x.at
		}
	}
	if latest.After(after) {
		err = p.store.Tx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE cerp_bridge.kind_cursor SET updated_after = $1 WHERE kind = 'project'`, latest)
			return err
		})
	}
	return n, err
}
