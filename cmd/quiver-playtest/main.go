package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"quiver-playtesting/internal/admin"
	"quiver-playtesting/internal/gateway"
	"quiver-playtesting/internal/live"
	"quiver-playtesting/internal/service"
	"quiver-playtesting/internal/store"
	"quiver-playtesting/internal/tui"
)

const usage = `usage: quiver-playtest <serve|tui> [flags]

  serve   run the public gateway and the admin socket
  tui     open the admin TUI (talks to a running serve over the socket)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "tui":
		err = runTUI(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// defaultDataDir keeps the data next to the real binary, so the command works
// from anywhere once it is symlinked onto the PATH.
func defaultDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "./data"
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), "data")
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", envOr("QP_LISTEN", "127.0.0.1:8480"), "public HTTP listen address (use a high port)")
	publicHost := fs.String("public-host", envOr("QP_PUBLIC_HOST", "playtesting.quiver.ar"), "public hostname, used for the Origin check")
	data := fs.String("data", envOr("QP_DATA", defaultDataDir()), "data directory (database and admin socket)")
	realIP := fs.String("real-ip-header", envOr("QP_REAL_IP_HEADER", "CF-Connecting-IP"), "header carrying the client IP, empty to use the socket peer")
	proxies := fs.String("trusted-proxies", envOr("QP_TRUSTED_PROXIES", "cloudflare"), "peers allowed to set the real IP header: comma separated CIDRs/IPs, 'cloudflare', or 'none'")
	maxConns := fs.Int("max-conns", 50, "max concurrent sessions")
	idle := fs.Duration("idle-timeout", 15*time.Minute, "end sessions with no traffic for this long")
	maxSession := fs.Duration("max-session", 4*time.Hour, "hard cap on a single session")
	retention := fs.Duration("log-retention", 90*24*time.Hour, "delete session history older than this, 0 keeps everything")
	fs.Parse(args)

	trusted, err := gateway.ParseTrustedProxies(*proxies)
	if err != nil {
		return err
	}
	if err := admin.PrepareDir(*data); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*data, "quiver.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	reg := live.New()
	svc := service.New(st, reg)

	srv := gateway.NewServer(gateway.Config{
		Listen:         *listen,
		PublicHost:     *publicHost,
		RealIPHeader:   *realIP,
		TrustedProxies: trusted,
		MaxConns:       *maxConns,
		IdleTimeout:    *idle,
		MaxSession:     *maxSession,
	}, svc, reg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *retention > 0 {
		go pruneLoop(ctx, st, *retention)
	}

	errc := make(chan error, 2)
	adminDone := make(chan struct{})
	go func() {
		slog.Info("gateway listening", "addr", srv.Addr, "trusted_proxies", len(trusted))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go func() {
		defer close(adminDone)
		if err := admin.Serve(ctx, filepath.Join(*data, "admin.sock"), svc); err != nil {
			errc <- err
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case runErr = <-errc:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && runErr == nil {
		runErr = err
	}
	<-adminDone
	return runErr
}

func pruneLoop(ctx context.Context, st *store.Store, keep time.Duration) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if n, err := st.PruneSessionLog(pctx, time.Now().Add(-keep)); err != nil {
			slog.Warn("session log prune failed", "err", err)
		} else if n > 0 {
			slog.Info("session log pruned", "rows", n)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func runTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	data := fs.String("data", envOr("QP_DATA", defaultDataDir()), "data directory of the running serve")
	publicURL := fs.String("public-url", envOr("QP_PUBLIC_URL", "https://playtesting.quiver.ar"), "base URL used to build playtester links")
	fs.Parse(args)

	c, err := admin.Dial(filepath.Join(*data, "admin.sock"))
	if err != nil {
		return fmt.Errorf("cannot reach the daemon (is serve running with the same --data?): %w", err)
	}
	defer c.Close()
	return tui.Run(c, *publicURL)
}
