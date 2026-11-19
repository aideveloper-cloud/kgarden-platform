// Command mes is the K Garden production shop-floor service (MES).
//
//	mes migrate   apply database migrations (uses MES_DB_OWNER_URL)
//	mes serve     run the HTTP API, Work Order sync (every MES_WORK_ORDER_SYNC, default 15s) and outbox worker (uses MES_DB_URL)
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
	worker := &Worker{store: store, erp: erp, sync: syncer, maxAttempts: 8,
		materialWait: durationEnv("MES_MATERIAL_RETRY", 10*time.Second)}

	if n, err := syncer.Run(ctx); err != nil {
		slog.Warn("initial work order sync failed", "err", err)
	} else {
		slog.Info("work orders synced", "count", n)
	}
	go worker.Loop(ctx, 2*time.Second)
	go func() {
		// New and changed Work Orders reach the tablets within this interval.
		t := time.NewTicker(durationEnv("MES_WORK_ORDER_SYNC", 15*time.Second))
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

	// Sign-in and access come from ERPNext (MES_FLOOR app); the service account only checks access.
	auth := &Auth{
		store:  store,
		access: erpnext.New(mustEnv("ERPNEXT_URL"), mustEnv("MES_ACCESS_API_KEY"), mustEnv("MES_ACCESS_API_SECRET")),
		oidc: OIDCConfig{
			ClientID:     mustEnv("MES_OIDC_CLIENT_ID"),
			ClientSecret: mustEnv("MES_OIDC_CLIENT_SECRET"),
			RedirectURI:  mustEnv("MES_OIDC_REDIRECT_URI"),
		},
		ttl:  durationEnv("MES_SESSION_TTL", 12*time.Hour),
		http: &http.Client{Timeout: 15 * time.Second},
	}
	go auth.WatchAccess(ctx, durationEnv("MES_ACCESS_POLL", time.Minute))

	srv := &http.Server{
		Addr:              mustEnv("MES_HTTP_ADDR"),
		Handler:           (&API{store: store, sync: syncer, auth: auth}).Routes(),
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

func durationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		fmt.Fprintf(os.Stderr, "%s: not a duration (e.g. 12h, 30s): %q\n", key, v)
		os.Exit(2)
	}
	return def
}
