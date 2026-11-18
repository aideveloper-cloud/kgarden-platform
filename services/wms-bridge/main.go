// Command wms-bridge copies the WMS (production-pos) stock ledger into ERPNext.
//
//	wms-bridge migrate   apply database migrations (uses MES_DB_OWNER_URL)
//	wms-bridge serve     read the WMS ledger, post Stock Entries to ERPNext, serve a status API
//
// The WMS stays the owner of physical movements (units, spools, FIFO); ERPNext becomes the core
// record of quantity and value. The bridge only reads the WMS database, with a read-only user.
package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
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
	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	wmsTimezone = envOr("WMS_TIMEZONE", "Asia/Bangkok")
	wmsLocation *time.Location
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing environment variable %s\n", key)
		os.Exit(2)
	}
	return v
}

func jsonNum(s string) json.Number { return json.Number(s) }

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: wms-bridge migrate|serve")
		os.Exit(2)
	}
	var err error
	if wmsLocation, err = time.LoadLocation(wmsTimezone); err != nil {
		fmt.Fprintln(os.Stderr, "WMS_TIMEZONE:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "migrate":
		err = pgstore.Migrate(ctx, mustEnv("MES_DB_OWNER_URL"), migrationFS, "migrations", "wms-bridge/")
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
	wms, err := sql.Open("mysql", mustEnv("WMS_DB_DSN"))
	if err != nil {
		return err
	}
	defer wms.Close()
	if err := wms.PingContext(ctx); err != nil {
		return fmt.Errorf("wms database: %w", err)
	}

	store := pgstore.New(pool, mustEnv("MES_ORG_ID"))
	erp := erpnext.New(mustEnv("ERPNEXT_URL"), mustEnv("ERPNEXT_API_KEY"), mustEnv("ERPNEXT_API_SECRET"))
	reader := &Reader{wms: wms, store: store}
	poster := &Poster{store: store, erp: erp, known: map[string]bool{}, cfg: Config{
		Company:          mustEnv("WMS_BRIDGE_COMPANY"),
		ParentWarehouse:  mustEnv("WMS_BRIDGE_PARENT_WAREHOUSE"),
		TransitWarehouse: mustEnv("WMS_BRIDGE_TRANSIT_WAREHOUSE"),
		WIPWarehouse:     os.Getenv("WMS_BRIDGE_WIP_WAREHOUSE"),
		FGStaging:        os.Getenv("WMS_BRIDGE_FG_STAGING_WAREHOUSE"),
		CreateMasters:    os.Getenv("WMS_BRIDGE_CREATE_MASTERS") == "1",
		MaxAttempts:      10,
	}}

	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			if n, err := reader.Poll(ctx); err != nil {
				slog.Error("read wms ledger", "err", err)
			} else if n > 0 {
				slog.Info("wms ledger rows read", "new", n)
			}
			if n, err := poster.Drain(ctx); err != nil {
				slog.Error("post to erpnext", "err", err)
			} else if n > 0 {
				slog.Info("ledger events handled", "count", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	srv := &http.Server{
		Addr:              envOr("WMS_BRIDGE_HTTP_ADDR", "127.0.0.1:8101"),
		Handler:           (&API{store: store}).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("wms-bridge listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
