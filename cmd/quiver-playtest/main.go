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
	"strconv"
	"strings"
	"syscall"
	"time"

	"quiver-playtesting/internal/admin"
	"quiver-playtesting/internal/gateway"
	"quiver-playtesting/internal/live"
	"quiver-playtesting/internal/recorder"
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

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
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
	continuous := fs.Bool("continuous-updates", envBool("QP_CONTINUOUS_UPDATES", true), "serve the RFB ContinuousUpdates extension (push-based updates); false copies bytes untouched")
	recordOn := fs.Bool("recordings", envBool("QP_RECORDINGS", true), "record every session to an mp4 (needs ffmpeg)")
	recordDir := fs.String("recordings-dir", envOr("QP_RECORDINGS_DIR", ""), "where recordings go, default <data>/recordings")
	ffmpegBin := fs.String("ffmpeg", envOr("QP_FFMPEG", "ffmpeg"), "ffmpeg binary")
	recordFPS := fs.Int("record-fps", envInt("QP_RECORD_FPS", 10), "recording frame rate, 1 to 30")
	recordMax := fs.Int("recordings-max", envInt("QP_RECORDINGS_MAX", 2), "max concurrent recordings")
	recordFree := fs.String("recordings-min-free", envOr("QP_RECORDINGS_MIN_FREE", "2GiB"), "stop recording below this much free disk")
	recordKeep := fs.Duration("recordings-retention", envDuration("QP_RECORDINGS_RETENTION", 720*time.Hour), "delete recordings older than this, 0 keeps everything")
	fs.Parse(args)

	if *recordFPS < 1 || *recordFPS > 30 {
		return fmt.Errorf("--record-fps must be between 1 and 30, got %d", *recordFPS)
	}
	if *recordMax < 1 {
		return fmt.Errorf("--recordings-max must be at least 1, got %d", *recordMax)
	}
	minFree, err := parseSize(*recordFree)
	if err != nil {
		return fmt.Errorf("--recordings-min-free: %w", err)
	}

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var rec *recorder.Recorder
	if *recordOn {
		dir := *recordDir
		if dir == "" {
			dir = filepath.Join(*data, "recordings")
		}
		rec = recorder.New(recorder.Config{Dir: dir, FFmpeg: *ffmpegBin, FPS: *recordFPS, Max: *recordMax, MinFree: minFree})
		if rec.Enabled() && *recordKeep > 0 {
			go recordingPruneLoop(ctx, dir, *recordKeep)
		}
	}

	srv := gateway.NewServer(gateway.Config{
		Listen:         *listen,
		PublicHost:     *publicHost,
		RealIPHeader:   *realIP,
		TrustedProxies: trusted,
		MaxConns:       *maxConns,
		IdleTimeout:    *idle,
		MaxSession:     *maxSession,
		Recorder:       recorderOrNil(rec),

		ContinuousUpdates: *continuous,
	}, svc, reg)

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

func recorderOrNil(r *recorder.Recorder) gateway.Recorder {
	if r == nil {
		return nil
	}
	return r
}

func recordingPruneLoop(ctx context.Context, dir string, keep time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := recorder.Prune(dir, keep, time.Now()); err != nil {
			slog.Warn("recordings prune failed", "err", err)
		} else if n > 0 {
			slog.Info("recordings pruned", "files", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// parseSize reads a byte count such as 2GiB, 512MiB, 1GB or a plain number.
func parseSize(s string) (uint64, error) {
	units := []struct {
		suffix string
		mult   uint64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1000}}
	mult := uint64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
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
