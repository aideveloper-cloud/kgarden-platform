// Command cerp-bridge books ConstructionERP purchasing in ERPNext (decided 2026-11-18).
//
//	cerp-bridge migrate   apply database migrations (uses MES_DB_OWNER_URL)
//	cerp-bridge serve     poll ConstructionERP, keep ERPNext Purchase Orders in step, serve a status API
//
// ConstructionERP owns PR -> PO -> approval; ERPNext (company CERP_BRIDGE_COMPANY) owns suppliers,
// accounts and VAT. The bridge reads ConstructionERP with a read-only database role.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aideveloper-cloud/kgarden-platform/internal/erpnext"
	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing environment variable %s\n", key)
		os.Exit(2)
	}
	return v
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cerp-bridge migrate|serve")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "migrate":
		err = pgstore.Migrate(ctx, mustEnv("MES_DB_OWNER_URL"), migrationFS, "migrations", "cerp-bridge/")
	case "serve":
		err = serve(ctx)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context) error {
	pool, err := pgxpool.New(ctx, mustEnv("MES_DB_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	cerp, err := pgxpool.New(ctx, mustEnv("CERP_DB_URL"))
	if err != nil {
		return err
	}
	defer cerp.Close()
	if err := cerp.Ping(ctx); err != nil {
		return fmt.Errorf("constructionerp database: %w", err)
	}

	store := pgstore.New(pool, mustEnv("MES_ORG_ID"))
	erp := erpnext.New(mustEnv("ERPNEXT_URL"), mustEnv("CERP_ERPNEXT_API_KEY"), mustEnv("CERP_ERPNEXT_API_SECRET"))
	reader := &Reader{cerp: cerp, store: store}
	poster := &Poster{store: store, erp: erp, known: map[string]bool{}, cfg: Config{
		Company:       mustEnv("CERP_BRIDGE_COMPANY"),
		VATAccount:    mustEnv("CERP_BRIDGE_VAT_ACCOUNT"),
		SupplierGroup: mustEnv("CERP_BRIDGE_SUPPLIER_GROUP"),
		MaxAttempts:   10,
	}}

	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			if n, err := reader.Poll(ctx); err != nil {
				slog.Error("read constructionerp", "err", err)
			} else if n > 0 {
				slog.Info("constructionerp po versions", "changed", n)
			}
			if n, err := poster.Drain(ctx); err != nil {
				slog.Error("book in erpnext", "err", err)
			} else if n > 0 {
				slog.Info("purchase orders reconciled", "count", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	srv := &http.Server{Addr: envOr("CERP_BRIDGE_HTTP_ADDR", "127.0.0.1:8102"), Handler: (&API{store: store}).Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("cerp-bridge listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
