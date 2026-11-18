package main

import (
	"context"
	"embed"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func audit(ctx context.Context, tx pgx.Tx, actor, action, entity, entityID string, data any) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO mes.audit_log (org_id, actor, action, entity, entity_id, data)
		VALUES (mes.current_org(), $1, $2, $3, $4, $5)`, actor, action, entity, entityID, data)
	return err
}
