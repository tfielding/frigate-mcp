// Command frigate-mcp is a read-only MCP server for Frigate NVR alerts, events and snapshots.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works in the distroless image, which has no zoneinfo

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfielding/frigate-mcp/internal/frigate"
	"github.com/tfielding/frigate-mcp/internal/tools"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

type config struct {
	frigateURL, frigateUser, frigatePass string
	insecureTLS                          bool
	listen                               string
	timeout                              time.Duration
	token                                string
	opt                                  tools.Options
}

func loadConfig() (config, error) {
	c := config{
		frigateURL:  os.Getenv("FRIGATE_URL"),
		frigateUser: os.Getenv("FRIGATE_USERNAME"),
		frigatePass: os.Getenv("FRIGATE_PASSWORD"),
		listen:      env("LISTEN_ADDR", ":8091"),
		token:       os.Getenv("AUTH_TOKEN"),
		opt:         tools.Options{Allowed: map[string]bool{}, Location: time.Local, GridBoxes: true},
	}
	if c.frigateURL == "" {
		return c, errors.New("FRIGATE_URL is required (e.g. https://frigate:8971 or http://frigate:5000)")
	}
	if (c.frigateUser == "") != (c.frigatePass == "") {
		return c, errors.New("set both FRIGATE_USERNAME and FRIGATE_PASSWORD, or neither")
	}
	for _, name := range strings.Split(os.Getenv("CAMERAS"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.opt.Allowed[strings.ToLower(name)] = true
		}
	}
	var err error
	if c.opt.SnapshotHeight, err = strconv.Atoi(env("SNAPSHOT_HEIGHT", "720")); err != nil || c.opt.SnapshotHeight < 0 {
		return c, errors.New("SNAPSHOT_HEIGHT must be a non-negative integer (0 = native size)")
	}
	if c.timeout, err = duration("FRIGATE_TIMEOUT", "20s"); err != nil || c.timeout <= 0 {
		return c, errors.New("FRIGATE_TIMEOUT must be a positive duration, e.g. 20s or 1m (bare numbers are seconds)")
	}
	if v := os.Getenv("GRID_BOXES"); v != "" {
		if c.opt.GridBoxes, err = strconv.ParseBool(v); err != nil {
			return c, errors.New("GRID_BOXES must be true or false")
		}
	}
	if c.opt.GridLabelCorner, err = tools.ParseCorner(os.Getenv("GRID_LABEL_POSITION")); err != nil {
		return c, fmt.Errorf("GRID_LABEL_POSITION: %w", err)
	}
	if v := os.Getenv("GRID_CAMERA_LABELS"); v != "" {
		if c.opt.GridCamera, err = strconv.ParseBool(v); err != nil {
			return c, errors.New("GRID_CAMERA_LABELS must be true or false")
		}
	}
	if v := os.Getenv("FRIGATE_TLS_INSECURE"); v != "" {
		if c.insecureTLS, err = strconv.ParseBool(v); err != nil {
			return c, errors.New("FRIGATE_TLS_INSECURE must be true or false")
		}
	}
	return c, nil
}

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: frigate-mcp [-version]\n\nConfigured with environment variables; see the README.\n")
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("frigate-mcp", version)
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	src := frigate.New(cfg.frigateURL, cfg.frigateUser, cfg.frigatePass, cfg.timeout, cfg.insecureTLS)
	allowed := "all"
	if len(cfg.opt.Allowed) > 0 {
		allowed = fmt.Sprint(len(cfg.opt.Allowed))
	}
	slog.Info("starting", "version", version, "listen", cfg.listen, "frigate", cfg.frigateURL,
		"login", cfg.frigateUser != "", "cameras", allowed, "auth", cfg.token != "", "tz", cfg.opt.Location.String(), "timeout", cfg.timeout)
	if err := src.Ping(ctx); err != nil {
		slog.Warn("frigate not reachable yet", "err", err)
	}

	handler, ts := newHandler(ctx, src, cfg)
	go ts.Watch(ctx)
	srv := &http.Server{Addr: cfg.listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	slog.Info("shut down")
	return nil
}

func newHandler(ctx context.Context, src *frigate.Client, cfg config) (http.Handler, *tools.Toolset) {
	server := mcp.NewServer(&mcp.Implementation{Name: "frigate-mcp", Version: version}, nil)
	ts := tools.Register(ctx, server, src, cfg.opt)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", requireToken(cfg.token, mcpHandler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := src.Ping(r.Context()); err != nil {
			http.Error(w, "frigate unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	return mux, ts
}

// requireToken enforces "Authorization: Bearer <token>" when a token is configured.
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// duration reads a Go duration ("20s", "1m"); a bare number is taken as seconds.
func duration(key, def string) (time.Duration, error) {
	v := env(key, def)
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	return time.ParseDuration(v)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
