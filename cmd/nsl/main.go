// Command nsl serves the Numbers Station Listener site.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/m0lte/numbers-station-listener/data"
	"github.com/m0lte/numbers-station-listener/internal/api"
	"github.com/m0lte/numbers-station-listener/internal/config"
	"github.com/m0lte/numbers-station-listener/internal/directory"
	"github.com/m0lte/numbers-station-listener/internal/probe"
	"github.com/m0lte/numbers-station-listener/internal/provider"
	"github.com/m0lte/numbers-station-listener/internal/provider/ubersdr"
	"github.com/m0lte/numbers-station-listener/internal/pskr"
	"github.com/m0lte/numbers-station-listener/internal/rank"
	"github.com/m0lte/numbers-station-listener/internal/relay"
	"github.com/m0lte/numbers-station-listener/internal/schedule"
	"github.com/m0lte/numbers-station-listener/internal/stations"
	"github.com/m0lte/numbers-station-listener/internal/webui"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("NSL_LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

func run(log *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	ua := cfg.UserAgent()
	log.Info("starting", "version", config.Version, "listen", cfg.Listen, "userAgent", ua, "receiverAllow", cfg.ReceiverAllow)

	var cat *stations.Catalog
	if cfg.StationsFile != "" {
		cat, err = stations.Load(cfg.StationsFile)
	} else {
		cat, err = stations.Parse(data.StationsJSON)
	}
	if err != nil {
		return err
	}

	weights := rank.DefaultWeights()
	if cfg.PSKReporter {
		weights.PSKReporter = 0.1
	}
	if cfg.RankWeights != "" {
		if weights, err = rank.ParseWeights(cfg.RankWeights); err != nil {
			return err
		}
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	uber := ubersdr.New(ubersdr.Options{
		DirectoryURL: cfg.UberSDRDirectory,
		UserAgent:    ua,
		HTTPClient:   httpClient,
		Allow:        cfg.Allowed,
		Logger:       log.With("provider", "ubersdr"),
	})
	providers := []provider.Provider{uber}

	snapshot := ""
	if cfg.DataDir != "" {
		snapshot = filepath.Join(cfg.DataDir, "schedule.json")
	}
	sched := schedule.New(schedule.Config{
		BaseURL:      cfg.PriyomURL,
		UserAgent:    ua,
		Interval:     cfg.PriyomPoll,
		SnapshotPath: snapshot,
		HTTPClient:   httpClient,
	}, cat, log.With("svc", "schedule"))

	dir := directory.New(providers, directory.Options{Interval: cfg.DirectoryPoll, Logger: log.With("svc", "directory")})

	rel := relay.New(relay.Config{Grace: cfg.Grace, PerReceiverCap: cfg.PerReceiverCap}, log.With("svc", "relay"), providers...)

	var prober *probe.Prober
	if cfg.ProbeEnabled {
		prober = probe.New(probe.Options{Every: cfg.ProbeEvery, TopK: cfg.ProbeTopK, Logger: log.With("svc", "probe")}, providers...)
	}

	var pathOpen api.PathOpenFunc
	var pskrClient *pskr.Client
	if cfg.PSKReporter {
		store := pskr.NewStore(pskr.DefaultParams())
		pskrClient = pskr.NewClient(pskr.Config{Logger: log.With("svc", "pskr")}, store)
		pathOpen = store.PathOpen
	}

	srv := api.New(api.Deps{
		Config: cfg, Logger: log.With("svc", "api"), Catalog: cat,
		Schedule: sched, Directory: dir, Prober: prober, Relay: rel,
		Providers: providers, Weights: weights, PathOpen: pathOpen, Static: webui.FS(),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() { _ = sched.Run(ctx) }()
	go func() { _ = dir.Run(ctx) }()
	go rel.Run(ctx)
	go srv.Run(ctx)
	if prober != nil {
		go prober.Run(ctx, srv.ProbeTargets)
	}
	if pskrClient != nil {
		go func() { _ = pskrClient.Run(ctx) }()
	}

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 2)
	go func() { errc <- hs.ListenAndServe() }()

	var redirect *http.Server
	if cfg.RedirectListen != "" {
		_, port, err := net.SplitHostPort(cfg.Listen)
		if err != nil {
			return fmt.Errorf("NSL_LISTEN: %w", err)
		}
		redirect = &http.Server{Addr: cfg.RedirectListen, Handler: api.PortRedirect(port), ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- redirect.ListenAndServe() }()
		log.Info("redirecting", "from", cfg.RedirectListen, "toPort", port)
	}

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	// Close every upstream first so receivers see clean close frames even
	// if browsers are slow to go.
	rel.Close()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	if redirect != nil {
		_ = redirect.Shutdown(sctx)
	}
	return nil
}
