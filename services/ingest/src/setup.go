package main

import (
	"bufio"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// The annotated config, with the answers as template actions. Embedded rather
// than built up in Go: a config is read far more often than it is written, and
// the comments are most of its value.
//
//go:embed config.template.yaml
var configTemplate string

const (
	// ingestDir holds everything that belongs to one source tree: its config and
	// its state database. Keeping them beside the tree they describe makes a
	// source directory self-contained — point the binary at it and it is set up.
	//
	// Writing into the source looks wrong at first glance, but the scanner never
	// sees it: the default ignore list matches both ".*" and ".ingest*", and an
	// ignored directory is never descended into. The second rule is the one that
	// matters, because ".*" is the one somebody might relax. Sweep (scan.go)
	// skips directories outright, and in any case matches the sidecar prefix
	// ".ingest." — with the trailing dot, so this directory is never a candidate
	// for deletion.
	ingestDir  = ".ingest"
	configName = "config.yaml"
	stateName  = "state.db"

	// stignoreName is syncthing's per-folder ignore list. Without an entry there,
	// syncthing would treat a live SQLite database as content and push it out to
	// every phone that shares the folder.
	stignoreName = ".stignore"
	stignoreLine = "/" + ingestDir
)

// setupOptions is every answer the generator needs. Each has a flag and, when
// the flag is absent, a prompt.
type setupOptions struct {
	Source     string
	Dest       string
	Name       string
	DryRun     bool
	Syncthing  bool
	Transcode  bool
	MinBitrate int64
	Marker     bool

	Force bool
	Yes   bool
}

func defaultSetupOptions() setupOptions {
	return setupOptions{
		DryRun:     true,
		Syncthing:  true,
		Transcode:  true,
		MinBitrate: 8_000_000,
		Marker:     true,
	}
}

// runSetup writes <source>/.ingest/config.yaml, interviewing the user for
// anything not given on the command line.
func runSetup(args []string) error {
	opts := defaultSetupOptions()

	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: ingest setup [flags]

Writes <source>/.ingest/config.yaml, with the state database beside it.
Anything not passed as a flag is asked for.

`)
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.Source, "source", "", "source directory to watch")
	fs.StringVar(&opts.Dest, "dest", "", "destination directory (the folder Immich reads)")
	fs.StringVar(&opts.Name, "name", "", "job name, used to tag the logs (default: the source's base name)")
	fs.BoolVar(&opts.DryRun, "dry-run", opts.DryRun, "start in dry-run mode")
	fs.BoolVar(&opts.Syncthing, "syncthing", opts.Syncthing, "the source is a syncthing folder: add .ingest to its .stignore")
	fs.BoolVar(&opts.Transcode, "transcode", opts.Transcode, "transcode large videos rather than copying everything")
	fs.Int64Var(&opts.MinBitrate, "min-bitrate", opts.MinBitrate, "transcode videos above this many bits per second")
	fs.BoolVar(&opts.Marker, "marker", opts.Marker, "create the destination marker file")
	fs.BoolVar(&opts.Force, "force", false, "overwrite an existing config")
	fs.BoolVar(&opts.Yes, "yes", false, "never prompt; fail if a required answer is missing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	ask := newAsker(os.Stdin, os.Stderr, opts.Yes || !isTerminal(os.Stdin), fs)
	if err := interview(&opts, ask); err != nil {
		return err
	}
	return writeSetup(opts, os.Stdout)
}

// interview fills in whatever the flags left blank. A flag that was passed
// explicitly is never questioned, so a fully specified command line is
// non-interactive without needing -yes.
func interview(opts *setupOptions, ask *asker) error {
	if err := ask.path("source", &opts.Source, "source directory to watch", ""); err != nil {
		return err
	}
	src, err := checkDir("source", opts.Source)
	if err != nil {
		return err
	}
	opts.Source = src

	if err := ask.path("dest", &opts.Dest, "destination directory", storageDirFromEnvFile()); err != nil {
		return err
	}
	dst, err := checkDir("dest", opts.Dest)
	if err != nil {
		return err
	}
	opts.Dest = dst

	if nested(opts.Source, opts.Dest) || nested(opts.Dest, opts.Source) {
		return fmt.Errorf("source and dest must not contain each other (%s, %s)", opts.Source, opts.Dest)
	}

	if opts.Name == "" {
		opts.Name = filepath.Base(opts.Source)
	}
	if err := ask.str("name", &opts.Name, "job name, used to tag the logs"); err != nil {
		return err
	}
	if err := ask.yesNo("dry-run", &opts.DryRun, "start in dry-run mode (write nothing until you have read a run's output)"); err != nil {
		return err
	}
	if err := ask.yesNo("syncthing", &opts.Syncthing, "is "+filepath.Base(opts.Source)+" a syncthing folder (adds "+ingestDir+" to its "+stignoreName+")"); err != nil {
		return err
	}
	if err := ask.yesNo("transcode", &opts.Transcode, "transcode large videos to HEVC"); err != nil {
		return err
	}
	if opts.Transcode {
		if err := ask.bitrate("min-bitrate", &opts.MinBitrate, "transcode videos above this bitrate, in bits per second"); err != nil {
			return err
		}
		if opts.MinBitrate < 0 {
			return fmt.Errorf("min-bitrate must not be negative, got %d", opts.MinBitrate)
		}
	}
	return ask.yesNo("marker", &opts.Marker, "create the destination marker "+filepath.Join(opts.Dest, defaultConfig().DestMarker))
}

// writeSetup does the work the interview decided on. Split from interview so the
// tests can drive it directly with a fully populated setupOptions.
func writeSetup(opts setupOptions, out io.Writer) error {
	dir := filepath.Join(opts.Source, ingestDir)
	cfgPath := filepath.Join(dir, configName)

	if _, err := os.Stat(cfgPath); err == nil && !opts.Force {
		return fmt.Errorf("%s already exists; pass -force to overwrite it", cfgPath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	body, err := renderConfig(opts)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nwrote %s\n", cfgPath)
	fmt.Fprintf(out, "state database will be created at %s\n", filepath.Join(dir, stateName))

	if opts.Syncthing {
		added, err := addStignore(opts.Source)
		if err != nil {
			return err
		}
		path := filepath.Join(opts.Source, stignoreName)
		if added {
			fmt.Fprintf(out, "added %s to %s\n", stignoreLine, path)
		} else {
			fmt.Fprintf(out, "%s already ignores %s\n", path, stignoreLine)
		}
	}

	if opts.Marker {
		marker := filepath.Join(opts.Dest, defaultConfig().DestMarker)
		if err := touch(marker); err != nil {
			// Not fatal: the drive is often simply not mounted yet, and ingest
			// waits for the marker rather than exiting.
			fmt.Fprintf(out, "could not create %s: %v\n", marker, err)
			fmt.Fprintf(out, "create it once the drive is mounted:  touch %s\n", shellQuote(marker))
		} else {
			fmt.Fprintf(out, "created %s\n", marker)
		}
	}

	fmt.Fprintf(out, "\ntry it:\n\n    ingest -config %s -once\n\n", shellQuote(cfgPath))
	if opts.DryRun {
		fmt.Fprintf(out, "That is a dry run. Read what it says it would do, then set dry_run: false.\n")
	}
	return nil
}

// renderConfig produces the config body. Exported to the tests through the
// package, which is what makes the generated article — not a sample that can
// drift from it — the thing under test.
func renderConfig(opts setupOptions) (string, error) {
	t, err := template.New("config").Funcs(template.FuncMap{"yaml": yamlString}).Parse(configTemplate)
	if err != nil {
		return "", err
	}
	def := defaultConfig()
	marker := filepath.Join(opts.Dest, def.DestMarker)

	var b strings.Builder
	err = t.Execute(&b, map[string]any{
		"Name":             opts.Name,
		"Source":           opts.Source,
		"Dest":             opts.Dest,
		"DryRun":           opts.DryRun,
		"Transcode":        opts.Transcode,
		"MinBitrate":       opts.MinBitrate,
		"StatePath":        filepath.Join(opts.Source, ingestDir, stateName),
		"DestMarker":       def.DestMarker,
		"MarkerPath":       shellQuote(marker),
		"SourceQuoted":     shellQuote(opts.Source),
		"Workers":          def.Workers,
		"TranscodeWorkers": def.TranscodeWorkers,
	})
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

// yamlString renders a value as a double-quoted YAML scalar, so a path
// containing a colon, a leading dash or a stray # cannot change the document's
// structure.
func yamlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// addStignore appends our directory to the source's .stignore, and reports
// whether it had to. Rewriting the file is avoided on purpose: it is the user's,
// and it may carry patterns we know nothing about.
func addStignore(source string) (bool, error) {
	path := filepath.Join(source, stignoreName)
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		switch strings.TrimSpace(line) {
		case stignoreLine, ingestDir, ingestDir + "/", stignoreLine + "/":
			return false, nil
		}
	}

	body := string(raw)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += stignoreLine + "\n"
	return true, os.WriteFile(path, []byte(body), 0o644)
}

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// checkDir resolves a path to an absolute one and insists it is a directory
// that exists. Validate does the same later, but failing here means the answer
// can be corrected while the user is still answering questions.
func checkDir(what, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s is required", what)
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	abs = filepath.Clean(abs)
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory: %s", what, abs)
	}
	return abs, nil
}

// storageDirFromEnvFile offers the repo-root .env's STORAGE_DIR as the default
// destination. It is a suggestion and nothing more: the generated config carries
// the resolved path, so a miss here costs one typed answer rather than breaking
// anything. That is the whole reason the agent needs no environment.
func storageDirFromEnvFile() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	candidates := []string{".env", "../../.env", "../../../.env"}
	roots := []string{".", filepath.Dir(exe)}
	for _, root := range roots {
		for _, c := range candidates {
			if v := readEnvVar(filepath.Join(root, c), "STORAGE_DIR"); v != "" {
				return v
			}
		}
	}
	return ""
}

func readEnvVar(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var value string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		value = v // last wins, the same rule docker compose applies
	}
	return value
}

// asker prompts for the answers the flags did not supply. A flag that was passed
// is left alone, so `ingest setup -source X -dest Y` asks nothing at all even
// without -yes.
type asker struct {
	in    *bufio.Reader
	out   io.Writer
	quiet bool // -yes, or no terminal: take the defaults, fail on a missing one
	set   map[string]bool
}

func newAsker(in io.Reader, out io.Writer, quiet bool, fs *flag.FlagSet) *asker {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return &asker{in: bufio.NewReader(in), out: out, quiet: quiet, set: set}
}

// prompt returns the typed answer, or "" for "take the default".
func (a *asker) prompt(name, question, def string) (string, bool, error) {
	if a.set[name] || a.quiet {
		return "", false, nil
	}
	if def != "" {
		fmt.Fprintf(a.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(a.out, "%s: ", question)
	}
	line, err := a.in.ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(a.out)
			return "", false, nil
		}
		return "", false, err
	}
	line = strings.TrimSpace(line)
	return line, line != "", nil
}

func (a *asker) str(name string, val *string, question string) error {
	answer, ok, err := a.prompt(name, question, *val)
	if err != nil {
		return err
	}
	if ok {
		*val = answer
	}
	return nil
}

// path keeps asking while the answer is empty and there is no default, since a
// config without a source is not a config.
func (a *asker) path(name string, val *string, question, def string) error {
	if *val == "" {
		*val = def
	}
	for i := 0; ; i++ {
		answer, ok, err := a.prompt(name, question, *val)
		if err != nil {
			return err
		}
		if ok {
			*val = answer
		}
		if *val != "" {
			return nil
		}
		if a.set[name] || a.quiet {
			return fmt.Errorf("%s is required: pass -%s", name, name)
		}
		if i >= 2 {
			return fmt.Errorf("%s is required", name)
		}
	}
}

func (a *asker) yesNo(name string, val *bool, question string) error {
	// Capital marks the default, the convention every other y/n prompt uses.
	def := "y/N"
	if *val {
		def = "Y/n"
	}
	answer, ok, err := a.prompt(name, question+"?", def)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	switch strings.ToLower(answer) {
	case "y", "yes", "true", "1":
		*val = true
	case "n", "no", "false", "0":
		*val = false
	default:
		return fmt.Errorf("-%s: expected yes or no, got %q", name, answer)
	}
	return nil
}

func (a *asker) bitrate(name string, val *int64, question string) error {
	answer, ok, err := a.prompt(name, question, strconv.FormatInt(*val, 10))
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(answer), 10, 64)
	if err != nil {
		return fmt.Errorf("-%s: expected a number of bits per second, got %q", name, answer)
	}
	*val = n
	return nil
}
