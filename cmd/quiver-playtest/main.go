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

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", envOr("QP_LISTEN", "127.0.0.1:8480"), "public HTTP listen address (use a high port)")
	publicHost := fs.String("public-host", envOr("QP_PUBLIC_HOST", "playtesting.quiver.ar"), "public hostname, used for the Origin check")
	data := fs.String("data", envOr("QP_DATA", "./data"), "data directory (database and admin socket)")
	realIP := fs.String("real-ip-header", envOr("QP_REAL_IP_HEADER", "CF-Connecting-IP"), "header carrying the client IP, empty to use the socket peer")
	maxConns := fs.Int("max-conns", 50, "max concurrent sessions")
	idle := fs.Duration("idle-timeout", 15*time.Minute, "end sessions with no traffic for this long")
	fs.Parse(args)

	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*data, "quiver.db"))
	if err != nil {
		return err
	}
	reg := live.New()
	svc := service.New(st, reg)

	srv := gateway.NewServer(gateway.Config{
		Listen:       *listen,
		PublicHost:   *publicHost,
		RealIPHeader: *realIP,
		MaxConns:     *maxConns,
		IdleTimeout:  *idle,
	}, svc, reg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 2)
	go func() {
		slog.Info("gateway listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go func() { errc <- admin.Serve(ctx, filepath.Join(*data, "admin.sock"), svc) }()

	select {
	case <-ctx.Done():
	case err := <-errc:
		stop()
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func runTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	data := fs.String("data", envOr("QP_DATA", "./data"), "data directory of the running serve")
	publicURL := fs.String("public-url", envOr("QP_PUBLIC_URL", "https://playtesting.quiver.ar"), "base URL used to build playtester links")
	fs.Parse(args)

	c, err := admin.Dial(filepath.Join(*data, "admin.sock"))
	if err != nil {
		return fmt.Errorf("cannot reach the daemon (is serve running with the same --data?): %w", err)
	}
	defer c.Close()
	return tui.Run(c, *publicURL)
}
