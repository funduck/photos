// Command ingest watches a source directory, transcodes large videos, and
// mirrors the result into a destination directory.
//
// It replaces the container syncthing hop in this repo's pipeline (host temp
// folder -> $STORAGE_DIR on the external drive). Unlike every other service
// here it runs as a host binary rather than a container, because
// hevc_videotoolbox — the hardware encoder that makes transcoding worth doing —
// is unreachable from a Linux container on Docker Desktop.
//
// State lives in SQLite, keyed by source-relative path plus size, so files can
// be deleted from either side without being copied again.
//
// See README.md for configuration and deployment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintln(os.Stderr, "ingest:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "config.yaml", "path to the YAML config file")
		once       = flag.Bool("once", false, "run a single pass and exit, instead of watching")
		report     = flag.Bool("report", false, "print outstanding work and failures, then exit")
		verbose    = flag.Bool("v", false, "with -report, list every planned file rather than just totals")
		logLevel   = flag.String("log-level", "info", "debug, info, warn or error")
		dryRun     = flag.String("dry-run", "", "override the config's dry_run (true or false)")
		dontAsk    = flag.Bool("dont-ask", false,
			"skip the confirmation prompt; implied when stdin is not a terminal")
		stopBefore = flag.Bool("stop-before-action", false,
			"scan and record every file as pending, but copy or transcode nothing; a later regular run drains the queue")
	)
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *dryRun != "" {
		v, perr := parseBool(*dryRun)
		if perr != nil {
			return fmt.Errorf("-dry-run: %w", perr)
		}
		cfg.DryRun = v
	}
	cfg.StopBeforeAction = *stopBefore
	cfg.DontAsk = *dontAsk
	if cfg.StopBeforeAction && cfg.DryRun {
		// One writes nothing at all, the other writes rows on purpose.
		return errors.New("-stop-before-action needs -dry-run=false: a dry run records nothing")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := newLogger(*logLevel)

	dbPath, err := StateDBPath(cfg.State, cfg.Source)
	if err != nil {
		return err
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once the first signal has been seen, restore default handling so a second
	// Ctrl-C kills the process outright. Without this, a shutdown that wedges
	// anywhere leaves no way out but SIGKILL from another terminal.
	go func() {
		<-ctx.Done()
		stop()
	}()

	if *report {
		return printReport(ctx, store, *verbose)
	}
	return serve(ctx, cfg, store, log, *once)
}

func serve(ctx context.Context, cfg Config, store *Store, log *slog.Logger, once bool) error {
	filter, err := NewFilter(cfg.Ignore)
	if err != nil {
		return err
	}

	queue := NewQueue(cfg.Scan.QueueSize)
	scanner := NewScanner(cfg.Source, filter, queue, log)
	transcoder := NewTranscoder(cfg.Transcode, cfg.Tools, NewExecRunner(cfg.Tools.Timeout.D()), log)
	pipeline := NewPipeline(cfg, store, filter, transcoder, log)

	logBanner(ctx, cfg, store, log, once)

	// Before the sweep, which deletes files, and before any work.
	if err := confirm(ctx, cfg, store, once, log); err != nil {
		return err
	}

	if !cfg.DryRun {
		// Anything half-written belongs to a run that died; nothing is running
		// yet, so all of it is safe to clear. The destination is swept too:
		// that is where an interrupted encode leaves real bytes behind.
		for _, root := range []string{cfg.Source, cfg.Dest} {
			n, err := Sweep(root, cfg.Transcode.SidecarPrefix)
			if err != nil {
				log.Warn("sweep failed", "root", root, "err", err)
			} else if n > 0 {
				log.Info("cleared interrupted work", "root", root, "count", n)
			}
		}
	}

	if err := waitForDest(ctx, cfg, log); err != nil {
		return err
	}

	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < cfg.Workers; i++ {
		g.Go(func() error {
			for {
				rel, ok := queue.Next(gctx)
				if !ok {
					return nil
				}
				pipeline.Process(gctx, rel)
				queue.Done(rel)
			}
		})
	}

	if once {
		if err := scanner.Scan(gctx); err != nil && gctx.Err() == nil {
			return err
		}
		// Workers drain what is queued, then Next returns on the closed channel.
		queue.Close()
		err := g.Wait()
		return finish(ctx, pipeline, log, err)
	}

	g.Go(func() error { return rescanLoop(gctx, cfg, scanner, log) })

	if cfg.Scan.Watch {
		watcher, werr := NewWatcher(cfg.Source, filter, queue, cfg.Scan.Debounce.D(), log)
		if werr != nil {
			// Not fatal: the rescan alone is correct, just slower.
			log.Warn("could not start the watcher, falling back to periodic rescans", "err", werr)
		} else {
			g.Go(func() error { return watcher.Run(gctx) })
		}
	}

	err = g.Wait()
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return finish(ctx, pipeline, log, err)
}

// rescanLoop runs the full walk immediately and then on every interval. It is
// the safety net for dropped watcher events, files that landed while the
// service was down, and failures whose backoff has expired.
func rescanLoop(ctx context.Context, cfg Config, scanner *Scanner, log *slog.Logger) error {
	scan := func() {
		if err := destReady(cfg.Dest, cfg.DestMarker); err != nil {
			log.Warn("destination not ready, skipping this scan", "dest", cfg.Dest, "err", err)
			return
		}
		if err := scanner.Scan(ctx); err != nil && ctx.Err() == nil {
			log.Error("scan failed", "err", err)
		}
	}
	scan()

	if cfg.Scan.Interval.D() <= 0 {
		<-ctx.Done()
		return nil
	}
	t := time.NewTicker(cfg.Scan.Interval.D())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			scan()
		}
	}
}

// waitForDest blocks until the destination marker exists. It deliberately does
// not exit: on macOS the drive is often just not mounted yet, and telling the
// user what to run beats making them read the logs of a crash loop.
func waitForDest(ctx context.Context, cfg Config, log *slog.Logger) error {
	err := destReady(cfg.Dest, cfg.DestMarker)
	if err == nil {
		return nil
	}

	// Printed rather than logged: slog quotes and escapes an attribute value,
	// which turns the command into something you cannot paste into a shell.
	fmt.Fprintf(os.Stderr, `
destination not ready: %v
  %s
  if the drive is mounted and this is the right directory, run:

      touch %s

  waiting, re-checking every %s (Ctrl-C to abort)

`, err, cfg.Dest, shellQuote(filepath.Join(cfg.Dest, cfg.DestMarker)), cfg.DestWait.D())

	t := time.NewTicker(cfg.DestWait.D())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := destReady(cfg.Dest, cfg.DestMarker); err != nil {
				log.Info("still waiting for the destination", "err", err)
				continue
			}
			log.Info("destination ready", "dest", cfg.Dest)
			return nil
		}
	}
}

// confirm shows what is about to happen and waits for the user to agree.
//
// It only asks when something will actually be written — a dry run needs no
// gate, and prompting there would only train you to hit "y" reflexively — and
// only when stdin is a terminal. That last part is the important one: under
// launchd there is nobody to answer, so a prompt would block forever and the
// service would hang, be restarted, and hang again.
func confirm(ctx context.Context, cfg Config, store *Store, once bool, log *slog.Logger) error {
	if cfg.DontAsk || cfg.DryRun {
		return nil
	}
	if !isTerminal(os.Stdin) {
		log.Info("not a terminal, continuing without confirmation")
		return nil
	}

	mode := "watch the source and process continuously"
	switch {
	case cfg.StopBeforeAction:
		mode = "record what needs doing, then stop (no files written)"
	case once:
		mode = "process everything once, then exit"
	}

	fmt.Fprintf(os.Stderr, `
about to run, for real — this writes to the destination
  from:     %s
  to:       %s
  state:    %s
  plan:     %s
  encoding: %s
`, cfg.Source, cfg.Dest, store.Path(), mode, encodingSummary(cfg))

	fmt.Fprint(os.Stderr, "\ncontinue? [y/N] ")

	// Read on a goroutine so a signal can cut the wait short. Because SIGINT is
	// trapped for graceful shutdown, its default "kill the process" behaviour is
	// gone — a bare blocking read here would ignore Ctrl-C entirely and hang.
	// The goroutine is left blocked on stdin, which is fine: either the user
	// answers, or the process is on its way out.
	answers := make(chan string, 1)
	go func() {
		var a string
		if _, err := fmt.Fscanln(os.Stdin, &a); err != nil {
			a = "" // EOF or a bare newline both mean "no"
		}
		answers <- a
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr)
		return errors.New("cancelled")
	case a := <-answers:
		switch strings.ToLower(strings.TrimSpace(a)) {
		case "y", "yes":
			return nil
		default:
			fmt.Fprintln(os.Stderr)
			return errors.New("cancelled")
		}
	}
}

func encodingSummary(cfg Config) string {
	if !cfg.Transcode.Enabled {
		return "off — every file is copied unchanged"
	}
	names := make([]string, 0, len(cfg.Transcode.Rules))
	for _, r := range cfg.Transcode.Rules {
		names = append(names, fmt.Sprintf("%s (%s above %d Mbps)",
			r.Name, strings.Join(r.Match.Extensions, " "), r.Match.MinVideoBitrate/1_000_000))
	}
	return strings.Join(names, ", ")
}

// isTerminal reports whether f is attached to a terminal, which is how we tell
// an interactive run from launchd, a pipe or a script.
//
// This needs a real tty check rather than os.ModeCharDevice: /dev/null is a
// character device too, and that is exactly what launchd hands a service as
// stdin. Getting it wrong means the daemon prompts nobody, reads EOF, exits,
// and gets restarted forever.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// finish prints the run summary and turns a failed file into a non-zero exit,
// so a -once test run is scriptable.
func finish(ctx context.Context, p *Pipeline, log *slog.Logger, err error) error {
	s := p.Stats().Snapshot()
	if ctx.Err() != nil {
		log.Info("interrupted", s.LogArgs()...)
		return nil
	}
	log.Info("run complete", s.LogArgs()...)
	if err != nil {
		return err
	}
	if s.Failed > 0 {
		return fmt.Errorf("%d file(s) failed to ingest", s.Failed)
	}
	return nil
}

func logBanner(ctx context.Context, cfg Config, store *Store, log *slog.Logger, once bool) {
	mode := "watch"
	if once {
		mode = "once"
	}
	log.Info("starting",
		"mode", mode, "dry_run", cfg.DryRun, "stop_before_action", cfg.StopBeforeAction,
		"source", cfg.Source, "dest", cfg.Dest,
		"state_db", store.Path(),
		"workers", cfg.Workers, "transcode_workers", cfg.TranscodeWorkers,
		"scan_interval", cfg.Scan.Interval.D(), "watch", cfg.Scan.Watch)

	// Log the encoder version: launchd does not inherit a shell PATH, and a
	// missing Homebrew ffmpeg is by far the most likely deployment failure.
	if res, err := NewExecRunner(30*time.Second).Run(ctx, cfg.Tools.FFmpeg, []string{"-version"}); err == nil {
		log.Info("ffmpeg", "version", firstLine(res.Stdout))
	}

	if counts, err := store.Counts(ctx); err == nil && len(counts) > 0 {
		args := make([]any, 0, len(counts)*2)
		for _, k := range []string{StatusDone, StatusPending, StatusFailed, StatusFailedPermanent} {
			if n, ok := counts[k]; ok {
				args = append(args, k, n)
			}
		}
		log.Info("state", args...)
	}
}

// printReport shows what is outstanding: work an inventory run has planned but
// not yet done, and anything that failed.
func printReport(ctx context.Context, store *Store, verbose bool) error {
	pending, err := store.Pending(ctx)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		var files int
		var bytes int64
		fmt.Println("pending work")
		for _, w := range pending {
			fmt.Printf("  %-10s %6d files  %10s\n", w.Action, w.Files, humanBytes(w.Bytes))
			files += w.Files
			bytes += w.Bytes
		}
		if len(pending) > 1 {
			fmt.Printf("  %-10s %6d files  %10s\n", "total", files, humanBytes(bytes))
		}
		fmt.Println()

		if verbose {
			rows, err := store.PendingFiles(ctx)
			if err != nil {
				return err
			}
			for _, r := range rows {
				fmt.Printf("  %-10s %10s %11s  %s%s\n",
					r.Action, humanBytes(r.Size), humanBitrate(r.Bitrate), r.RelPath, suffixReason(r.Reason))
			}
			fmt.Println()
		}
	}

	failures, err := store.Failures(ctx)
	if err != nil {
		return err
	}
	if len(failures) > 0 {
		fmt.Println("failures")
		for _, f := range failures {
			fmt.Printf("  %s  %-16s attempts=%d  %s\n",
				time.Unix(f.UpdatedAt, 0).Format(time.RFC3339), f.Status, f.Attempts, f.RelPath)
			fmt.Printf("      %s\n", f.LastError)
		}
		fmt.Printf("\n%d failure(s)\n", len(failures))
	}

	if len(pending) == 0 && len(failures) == 0 {
		fmt.Println("nothing pending, no failures")
	}
	return nil
}

// humanBitrate renders a probed bitrate, or blank when ffprobe could not
// determine one — a column of "0 Mbps" would read as a fact rather than a gap.
func humanBitrate(b int64) string {
	if b <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1f Mbps", float64(b)/1e6)
}

func suffixReason(reason string) string {
	if reason == "" {
		return ""
	}
	return "  (" + reason + ")"
}

// humanBytes formats a size the way the transcode report does, so the two read
// the same.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %sB", float64(b)/float64(div), "KMGTP"[exp:exp+1])
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

// shellQuote makes a path safe to paste into a shell, quoting only when the
// path actually needs it so the common case stays readable.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`*?[]|&;<>()#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
