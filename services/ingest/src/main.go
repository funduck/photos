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

// stringList collects a flag that may be repeated on the command line.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, " ") }

func (l *stringList) Set(v string) error {
	if v == "" {
		return errors.New("empty path")
	}
	*l = append(*l, v)
	return nil
}

// job is one source tree: its own config, state database and logger. Several
// jobs run side by side in one process so that they share a single encode
// limiter — see newEncodeLimiter.
type job struct {
	cfg   Config
	store *Store
	log   *slog.Logger
}

func run() error {
	// One subcommand, dispatched before flag.Parse so that setup's flags and the
	// daemon's cannot collide.
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		return runSetup(os.Args[2:])
	}

	var configPaths stringList
	flag.Var(&configPaths, "config",
		"path to a YAML config file; repeat it to ingest several source trees in one process")
	var (
		once     = flag.Bool("once", false, "run a single pass and exit, instead of watching")
		report   = flag.Bool("report", false, "print outstanding work and failures, then exit")
		verbose  = flag.Bool("v", false, "with -report, list every planned file rather than just totals")
		logLevel = flag.String("log-level", "info", "debug, info, warn or error")
		dryRun   = flag.String("dry-run", "", "override every config's dry_run (true or false)")
		dontAsk  = flag.Bool("dont-ask", false,
			"skip the confirmation prompt; implied when stdin is not a terminal")
		stopBefore = flag.Bool("stop-before-action", false,
			"scan and record every file as pending, but copy or transcode nothing; a later regular run drains the queue")
	)
	flag.Parse()

	if len(configPaths) == 0 {
		configPaths = stringList{"config.yaml"}
	}
	dryRunSet, dryRunVal := false, false
	if *dryRun != "" {
		v, err := parseBool(*dryRun)
		if err != nil {
			return fmt.Errorf("-dry-run: %w", err)
		}
		dryRunSet, dryRunVal = true, v
	}

	log := newLogger(*logLevel)

	var jobs []job
	// Named so every store opened before a later config failed still closes.
	defer func() {
		for _, j := range jobs {
			j.store.Close()
		}
	}()

	for _, path := range configPaths {
		cfg, err := LoadConfig(path)
		if err != nil {
			return err
		}
		if dryRunSet {
			cfg.DryRun = dryRunVal
		}
		cfg.StopBeforeAction = *stopBefore
		cfg.DontAsk = *dontAsk
		if cfg.StopBeforeAction && cfg.DryRun {
			// One writes nothing at all, the other writes rows on purpose.
			return fmt.Errorf("%s: -stop-before-action needs -dry-run=false: a dry run records nothing", cfg.Name)
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("%s: %w", cfg.Name, err)
		}

		dbPath, err := StateDBPath(cfg.State, cfg.Source)
		if err != nil {
			return fmt.Errorf("%s: %w", cfg.Name, err)
		}
		store, err := OpenStore(dbPath)
		if err != nil {
			return fmt.Errorf("%s: %w", cfg.Name, err)
		}
		jobs = append(jobs, job{cfg: cfg, store: store, log: log.With("job", cfg.Name)})
	}

	cfgs := make([]Config, len(jobs))
	for i, j := range jobs {
		cfgs[i] = j.cfg
	}
	if err := ValidateJobs(cfgs); err != nil {
		return err
	}

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
		return printReport(ctx, jobs, *verbose)
	}
	return serveAll(ctx, jobs, newEncodeLimiter(jobs, log), log, *once)
}

// newEncodeLimiter builds the one semaphore every job's transcode step passes
// through. VideoToolbox is a single hardware engine, so the limit has to be
// process-wide: running a second agent per source tree — the obvious way to
// ingest two trees — would quietly double the number of concurrent encodes,
// each config still believing its transcode_workers: 1 was being honoured.
//
// When the configs disagree the smallest wins. It is the only choice that can
// never over-subscribe the encoder, and it needs no config edit to be right.
func newEncodeLimiter(jobs []job, log *slog.Logger) chan struct{} {
	n := jobs[0].cfg.TranscodeWorkers
	differ := false
	for _, j := range jobs[1:] {
		if j.cfg.TranscodeWorkers != n {
			differ = true
		}
		if j.cfg.TranscodeWorkers < n {
			n = j.cfg.TranscodeWorkers
		}
	}
	if differ {
		log.Warn("configs disagree on transcode_workers; the limit is shared by every job, so the smallest wins",
			"transcode_workers", n)
	}
	return make(chan struct{}, n)
}

func serveAll(ctx context.Context, jobs []job, encode chan struct{}, log *slog.Logger, once bool) error {
	logBanner(ctx, jobs, encode, log, once)

	// Before the sweeps, which delete files, and before any work.
	if err := confirm(ctx, jobs, once, log); err != nil {
		return err
	}

	g, gctx := errgroup.WithContext(ctx)
	pipelines := make([]*Pipeline, len(jobs))
	for i, j := range jobs {
		p, err := startJob(gctx, g, j, encode, once)
		if err != nil {
			return err
		}
		pipelines[i] = p
	}

	err := g.Wait()
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return finish(ctx, jobs, pipelines, log, err)
}

// startJob wires up one source tree and launches it on g. Everything that can
// block — waiting for the drive, scanning, encoding — happens on the group, so
// a job whose destination is unmounted never holds up the others.
func startJob(ctx context.Context, g *errgroup.Group, j job, encode chan struct{}, once bool) (*Pipeline, error) {
	filter, err := NewFilter(j.cfg.Ignore)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", j.cfg.Name, err)
	}

	queue := NewQueue(j.cfg.Scan.QueueSize)
	scanner := NewScanner(j.cfg.Source, filter, queue, j.log)
	transcoder := NewTranscoder(j.cfg.Transcode, j.cfg.Tools, NewExecRunner(j.cfg.Tools.Timeout.D()), j.log)
	pipeline := NewPipeline(j.cfg, j.store, filter, transcoder, encode, j.log)

	g.Go(func() error {
		if !j.cfg.DryRun {
			// Anything half-written belongs to a run that died; nothing is
			// running yet, so all of it is safe to clear. The destination is
			// swept too: that is where an interrupted encode leaves real bytes.
			for _, root := range []string{j.cfg.Source, j.cfg.Dest} {
				n, err := Sweep(root, j.cfg.Transcode.SidecarPrefix)
				if err != nil {
					j.log.Warn("sweep failed", "root", root, "err", err)
				} else if n > 0 {
					j.log.Info("cleared interrupted work", "root", root, "count", n)
				}
			}
		}

		if err := waitForDest(ctx, j.cfg, j.log); err != nil {
			return err
		}

		wg, wctx := errgroup.WithContext(ctx)
		for i := 0; i < j.cfg.Workers; i++ {
			wg.Go(func() error {
				for {
					rel, ok := queue.Next(wctx)
					if !ok {
						return nil
					}
					pipeline.Process(wctx, rel)
					queue.Done(rel)
				}
			})
		}

		if once {
			// Workers drain what is queued, then Next returns on the closed
			// channel. The close has to happen even on a failed scan, or they
			// would block forever waiting for work that is not coming.
			serr := scanner.Scan(wctx)
			queue.Close()
			if werr := wg.Wait(); werr != nil {
				return werr
			}
			if serr != nil && wctx.Err() == nil {
				return serr
			}
			return nil
		}

		wg.Go(func() error { return rescanLoop(wctx, j.cfg, scanner, j.log) })

		if j.cfg.Scan.Watch {
			watcher, werr := NewWatcher(j.cfg.Source, filter, queue, j.cfg.Scan.Debounce.D(), j.log)
			if werr != nil {
				// Not fatal: the rescan alone is correct, just slower.
				j.log.Warn("could not start the watcher, falling back to periodic rescans", "err", werr)
			} else {
				wg.Go(func() error { return watcher.Run(wctx) })
			}
		}

		return wg.Wait()
	})

	return pipeline, nil
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
//
// One prompt covers every job: the question is whether this process should run,
// not whether each tree should.
func confirm(ctx context.Context, jobs []job, once bool, log *slog.Logger) error {
	writes := false
	for _, j := range jobs {
		if j.cfg.DontAsk {
			return nil
		}
		if !j.cfg.DryRun {
			writes = true
		}
	}
	if !writes {
		return nil
	}
	if !isTerminal(os.Stdin) {
		log.Info("not a terminal, continuing without confirmation")
		return nil
	}

	fmt.Fprintln(os.Stderr, "\nabout to run, for real — this writes to the destination")
	for _, j := range jobs {
		fmt.Fprintf(os.Stderr, `
  %s
    from:     %s
    to:       %s
    state:    %s
    plan:     %s
    encoding: %s
`, j.cfg.Name, j.cfg.Source, j.cfg.Dest, j.store.Path(), planSummary(j.cfg, once), encodingSummary(j.cfg))
	}

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

func planSummary(cfg Config, once bool) string {
	switch {
	case cfg.DryRun:
		return "dry run — nothing is written"
	case cfg.StopBeforeAction:
		return "record what needs doing, then stop (no files written)"
	case once:
		return "process everything once, then exit"
	default:
		return "watch the source and process continuously"
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

// finish prints the per-job summaries plus a total, and turns a failed file
// into a non-zero exit, so a -once test run is scriptable.
func finish(ctx context.Context, jobs []job, pipelines []*Pipeline, log *slog.Logger, err error) error {
	var total StatsSnapshot
	failed := 0
	for i, p := range pipelines {
		s := p.Stats().Snapshot()
		total = total.plus(s)
		failed += s.Failed
		if ctx.Err() != nil {
			jobs[i].log.Info("interrupted", s.LogArgs()...)
		} else {
			jobs[i].log.Info("job complete", s.LogArgs()...)
		}
	}

	if ctx.Err() != nil {
		log.Info("interrupted", total.LogArgs()...)
		return nil
	}
	if len(pipelines) > 1 {
		log.Info("run complete", total.LogArgs()...)
	}
	if err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d file(s) failed to ingest", failed)
	}
	return nil
}

func logBanner(ctx context.Context, jobs []job, encode chan struct{}, log *slog.Logger, once bool) {
	mode := "watch"
	if once {
		mode = "once"
	}
	// transcode_workers is logged here rather than per job because it is now a
	// property of the process: one hardware encoder, one shared limit.
	log.Info("starting", "mode", mode, "jobs", len(jobs), "transcode_workers", cap(encode))

	// Log the encoder version: launchd does not inherit a shell PATH, and a
	// missing Homebrew ffmpeg is by far the most likely deployment failure.
	// Once per distinct binary — the configs almost always name the same one.
	seen := map[string]bool{}
	for _, j := range jobs {
		if seen[j.cfg.Tools.FFmpeg] {
			continue
		}
		seen[j.cfg.Tools.FFmpeg] = true
		if res, err := NewExecRunner(30*time.Second).Run(ctx, j.cfg.Tools.FFmpeg, []string{"-version"}); err == nil {
			log.Info("ffmpeg", "path", j.cfg.Tools.FFmpeg, "version", firstLine(res.Stdout))
		}
	}

	for _, j := range jobs {
		j.log.Info("job",
			"dry_run", j.cfg.DryRun, "stop_before_action", j.cfg.StopBeforeAction,
			"source", j.cfg.Source, "dest", j.cfg.Dest,
			"state_db", j.store.Path(),
			"workers", j.cfg.Workers,
			"scan_interval", j.cfg.Scan.Interval.D(), "watch", j.cfg.Scan.Watch)

		if counts, err := j.store.Counts(ctx); err == nil && len(counts) > 0 {
			args := make([]any, 0, len(counts)*2)
			for _, k := range []string{StatusDone, StatusPending, StatusFailed, StatusFailedPermanent} {
				if n, ok := counts[k]; ok {
					args = append(args, k, n)
				}
			}
			j.log.Info("state", args...)
		}
	}
}

// printReport shows what is outstanding: work an inventory run has planned but
// not yet done, and anything that failed. With several jobs each gets its own
// section, since the two trees have nothing to do with each other.
func printReport(ctx context.Context, jobs []job, verbose bool) error {
	for _, j := range jobs {
		if len(jobs) > 1 {
			fmt.Printf("== %s ==\n\n", j.cfg.Name)
		}
		if err := reportJob(ctx, j.store, verbose); err != nil {
			return fmt.Errorf("%s: %w", j.cfg.Name, err)
		}
		if len(jobs) > 1 {
			fmt.Println()
		}
	}
	return nil
}

func reportJob(ctx context.Context, store *Store, verbose bool) error {
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
