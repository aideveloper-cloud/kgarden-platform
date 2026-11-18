// Command mes is the K Garden production shop-floor service (MES).
//
//	mes migrate   apply database migrations (uses MES_DB_OWNER_URL)
//	mes serve     run the HTTP API, Work Order sync and outbox worker (uses MES_DB_URL)
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/aideveloper-cloud/kgarden-platform/internal/erpnext"
	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
		fmt.Fprintln(os.Stderr, "usage: mes migrate|serve")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "migrate":
		err = pgstore.Migrate(ctx, mustEnv("MES_DB_OWNER_URL"), migrationFS, "migrations", "")
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

	store := pgstore.New(pool, mustEnv("MES_ORG_ID"))
	erp := erpnext.New(mustEnv("ERPNEXT_URL"), mustEnv("ERPNEXT_API_KEY"), mustEnv("ERPNEXT_API_SECRET"))
	syncer := &Syncer{store: store, erp: erp}
	worker := &Worker{store: store, erp: erp, sync: syncer, maxAttempts: 8}

	if n, err := syncer.Run(ctx); err != nil {
		slog.Warn("initial work order sync failed", "err", err)
	} else {
		slog.Info("work orders synced", "count", n)
	}
	go worker.Loop(ctx, 2*time.Second)
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := syncer.Run(ctx); err != nil {
					slog.Warn("work order sync failed", "err", err)
				}
			}
		}
	}()

	srv := &http.Server{
		Addr:              mustEnv("MES_HTTP_ADDR"),
		Handler:           (&API{store: store, sync: syncer}).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("mes listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
