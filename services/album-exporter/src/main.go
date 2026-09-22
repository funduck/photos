// Command album-exporter copies the photos of Immich albums into plain
// directories, each asset once. See ../README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "album-exporter:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "/config/config.yaml", "path to the config file")
		once       = flag.Bool("once", false, "run a single pass and exit, instead of polling")
		report     = flag.Bool("report", false, "print per-job totals and failures, then exit")
		logLevel   = flag.String("log-level", "info", "debug, info, warn or error")
		dryRun     = flag.String("dry-run", "", "override the config's dry_run (true or false)")
	)
	flag.Parse()
	log := newLogger(*logLevel)

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *dryRun != "" {
		v, err := parseBool(*dryRun)
		if err != nil {
			return fmt.Errorf("-dry-run: %w", err)
		}
		cfg.DryRun = v
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	store, err := OpenStore(cfg.State.Path)
	if err != nil {
		return fmt.Errorf("open state %s: %w", cfg.State.Path, err)
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once the first signal has been seen, restore default handling so a second
	// one kills the process outright.
	go func() {
		<-ctx.Done()
		stop()
	}()

	if *report {
		return printReport(ctx, store, cfg)
	}

	e := &Exporter{
		cfg:    cfg,
		immich: NewImmich(cfg.ImmichURL, cfg.APIKey, nil),
		store:  store,
		log:    log,
		now:    time.Now,
	}
	for _, j := range cfg.Jobs {
		log.Info("job", "job", j.Name, "album", j.Album, "dest", j.Dest, "dry_run", cfg.DryRun)
	}
	if !cfg.DryRun {
		e.Sweep()
	}

	for {
		passAll(ctx, e)
		if *once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.PollInterval.D()):
		}
	}
}

// passAll runs one pass per job. A failing job is logged and retried next
// poll; it does not hold up the others.
func passAll(ctx context.Context, e *Exporter) {
	for _, j := range e.cfg.Jobs {
		res, err := e.Pass(ctx, j)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			e.log.Error("pass failed", "job", j.Name, "err", err)
			continue
		}
		if res.Exported+res.Planned+res.Failed > 0 {
			e.log.Info("pass done", "job", j.Name, "exported", res.Exported,
				"would_export", res.Planned, "failed", res.Failed, "unchanged", res.Skipped)
		}
	}
}

func printReport(ctx context.Context, store *Store, cfg Config) error {
	for _, j := range cfg.Jobs {
		counts, err := store.Counts(ctx, j.Name)
		if err != nil {
			return err
		}
		fmt.Printf("%s (%q -> %s)\n", j.Name, j.Album, j.Dest)
		fmt.Printf("  done %d, pending %d, retrying %d, parked %d\n",
			counts[StatusDone], counts[StatusPending], counts[StatusFailed], counts[StatusFailedPermanent])

		failures, err := store.Failures(ctx, j.Name)
		if err != nil {
			return err
		}
		for _, f := range failures {
			when := "parked"
			if f.Status == StatusFailed {
				when = "retry at " + time.Unix(f.NextRetry, 0).Format(time.DateTime)
			}
			fmt.Printf("  %s  %d attempts, %s: %s\n", f.AssetID, f.Attempts, when, firstLine(f.LastError))
		}
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func parseBool(s string) (bool, error) {
	switch s {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("expected true or false, got %q", s)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
