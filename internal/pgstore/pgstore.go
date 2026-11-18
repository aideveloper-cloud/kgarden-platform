// Package pgstore holds the PostgreSQL plumbing shared by the K Garden services: transactions
// scoped to one organisation (row-level security) and embedded SQL migrations.
package pgstore

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store runs every transaction with app.org_id set, so row-level security limits it to this
// organisation's rows.
type Store struct {
	pool  *pgxpool.Pool
	orgID string
}

func New(pool *pgxpool.Pool, orgID string) *Store { return &Store{pool: pool, orgID: orgID} }

func (s *Store) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, s.orgID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Migrate applies the *.sql files under dir in fsys in filename order, as the schema owner role.
// Applied files are recorded in public.schema_migrations as namespace+path.
func Migrate(ctx context.Context, ownerURL string, fsys fs.FS, dir, namespace string) error {
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(fsys, dir+"/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version := namespace + f
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := fs.ReadFile(fsys, f)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.schema_migrations (version) VALUES ($1)`, version); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Println("applied", version)
	}
	return nil
}
