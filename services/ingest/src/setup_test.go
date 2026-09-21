package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// setupTree builds a source and a destination and returns options pointing at
// them, fully specified so nothing prompts.
func setupTree(t *testing.T) setupOptions {
	t.Helper()
	root := t.TempDir()
	opts := defaultSetupOptions()
	opts.Source = filepath.Join(root, "src")
	opts.Dest = filepath.Join(root, "dst")
	opts.Name = "phones"
	for _, d := range []string{opts.Source, opts.Dest} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return opts
}

func runWriteSetup(t *testing.T, opts setupOptions) string {
	t.Helper()
	var out bytes.Buffer
	if err := writeSetup(opts, &out); err != nil {
		t.Fatalf("writeSetup: %v", err)
	}
	return out.String()
}

// The generated config is the thing users actually get, so it has to load and
// pass the same validation a hand-written one does.
func TestSetupWritesLoadableConfig(t *testing.T) {
	opts := setupTree(t)
	runWriteSetup(t, opts)

	path := filepath.Join(opts.Source, ingestDir, configName)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("generated config does not parse: %v", err)
	}
	if cfg.Name != "phones" {
		t.Errorf("name = %q, want phones", cfg.Name)
	}
	if cfg.Source != opts.Source || cfg.Dest != opts.Dest {
		t.Errorf("source/dest = %q, %q; want %q, %q", cfg.Source, cfg.Dest, opts.Source, opts.Dest)
	}
	if !cfg.DryRun {
		t.Error("generated config is not a dry run: setup must never arm a fresh install")
	}
	if len(cfg.Transcode.Rules) == 0 {
		t.Fatal("no transcode rules")
	}
	if got := cfg.Transcode.Rules[0].Match.MinVideoBitrate; got != opts.MinBitrate {
		t.Errorf("min_video_bitrate = %d, want %d", got, opts.MinBitrate)
	}

	// Validate resolves the external tools, which a CI box need not have.
	if _, err := exec.LookPath(cfg.Tools.FFmpeg); err != nil {
		t.Skipf("skipping Validate: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("generated config does not validate: %v", err)
	}
}

// The config and the state database go in the source tree, which only works
// because the scan never looks inside .ingest. If that ever stops being true,
// ingest would try to copy its own database into the library.
func TestSetupStateLivesInSourceAndIsIgnored(t *testing.T) {
	opts := setupTree(t)
	runWriteSetup(t, opts)

	cfg, err := LoadConfig(filepath.Join(opts.Source, ingestDir, configName))
	if err != nil {
		t.Fatal(err)
	}
	db, err := StateDBPath(cfg.State, cfg.Source)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(opts.Source, ingestDir, stateName); db != want {
		t.Errorf("state db = %q, want %q", db, want)
	}

	filter, err := NewFilter(cfg.Ignore)
	if err != nil {
		t.Fatal(err)
	}
	if !filter.IgnoreName(ingestDir) {
		t.Errorf("the scanner would descend into %s/, and copy its own state database", ingestDir)
	}

	// ".*" alone would do it, but it is the rule most likely to be relaxed --
	// wanting .hidden photos ingested is a reasonable thing to want. The
	// ".ingest*" rule is the backstop, and it only works because these are
	// globs: the dot is literal, so ".ingest.*" matches the sidecars but not
	// the directory holding the config and the database.
	var relaxed IgnoreConfig
	relaxed = cfg.Ignore
	relaxed.Names = nil
	for _, n := range cfg.Ignore.Names {
		if n != ".*" {
			relaxed.Names = append(relaxed.Names, n)
		}
	}
	if len(relaxed.Names) == len(cfg.Ignore.Names) {
		t.Fatalf("expected a %q rule in %v", ".*", cfg.Ignore.Names)
	}
	bare, err := NewFilter(relaxed)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ingestDir, ".ingest.VID.mp4.transcoded", ".ingest.VID.mp4.failed"} {
		if !bare.IgnoreName(name) {
			t.Errorf("without the %q rule, %q is no longer ignored: %v", ".*", name, relaxed.Names)
		}
	}
}

func TestSetupStignore(t *testing.T) {
	t.Run("created when missing", func(t *testing.T) {
		opts := setupTree(t)
		runWriteSetup(t, opts)
		if got := readStignore(t, opts.Source); got != stignoreLine+"\n" {
			t.Errorf("stignore = %q, want %q", got, stignoreLine+"\n")
		}
	})

	t.Run("appended to, keeping what is there", func(t *testing.T) {
		opts := setupTree(t)
		path := filepath.Join(opts.Source, stignoreName)
		if err := os.WriteFile(path, []byte("(?d).DS_Store"), 0o644); err != nil {
			t.Fatal(err)
		}
		runWriteSetup(t, opts)

		got := readStignore(t, opts.Source)
		if !strings.Contains(got, "(?d).DS_Store") {
			t.Errorf("setup dropped an existing pattern: %q", got)
		}
		if !strings.Contains(got, stignoreLine) {
			t.Errorf("stignore = %q, missing %q", got, stignoreLine)
		}
	})

	t.Run("second run changes nothing", func(t *testing.T) {
		opts := setupTree(t)
		runWriteSetup(t, opts)
		first := readStignore(t, opts.Source)

		opts.Force = true
		runWriteSetup(t, opts)
		if second := readStignore(t, opts.Source); second != first {
			t.Errorf("stignore changed on a second run: %q then %q", first, second)
		}
	})

	t.Run("left alone when not a syncthing folder", func(t *testing.T) {
		opts := setupTree(t)
		opts.Syncthing = false
		runWriteSetup(t, opts)
		if _, err := os.Stat(filepath.Join(opts.Source, stignoreName)); !os.IsNotExist(err) {
			t.Errorf("wrote a %s for a non-syncthing source", stignoreName)
		}
	})
}

func TestSetupMarker(t *testing.T) {
	opts := setupTree(t)
	runWriteSetup(t, opts)
	if _, err := os.Stat(filepath.Join(opts.Dest, defaultConfig().DestMarker)); err != nil {
		t.Errorf("marker not created: %v", err)
	}

	opts2 := setupTree(t)
	opts2.Marker = false
	runWriteSetup(t, opts2)
	if _, err := os.Stat(filepath.Join(opts2.Dest, defaultConfig().DestMarker)); !os.IsNotExist(err) {
		t.Error("marker created although -marker=false")
	}
}

// Overwriting a config would silently discard whatever the user had tuned.
func TestSetupRefusesToOverwrite(t *testing.T) {
	opts := setupTree(t)
	runWriteSetup(t, opts)

	if err := writeSetup(opts, io.Discard); err == nil {
		t.Fatal("setup overwrote an existing config without -force")
	}

	opts.Force = true
	opts.Name = "renamed"
	runWriteSetup(t, opts)
	cfg, err := LoadConfig(filepath.Join(opts.Source, ingestDir, configName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "renamed" {
		t.Errorf("-force did not rewrite the config: name = %q", cfg.Name)
	}
}

// config.example.yaml documents the same format the generator emits. Two
// annotated copies of one format can drift; this keeps them honest at the level
// that matters — which keys exist at all.
func TestGeneratedConfigMatchesExample(t *testing.T) {
	opts := setupTree(t)
	body, err := renderConfig(opts)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join("..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	gen, ex := topLevelKeys(t, body), topLevelKeys(t, string(raw))
	if strings.Join(gen, " ") != strings.Join(ex, " ") {
		t.Errorf("generated config and config.example.yaml describe different configs:\n  generated: %v\n  example:   %v", gen, ex)
	}
}

func topLevelKeys(t *testing.T, doc string) []string {
	t.Helper()
	var m map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(doc)), &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func readStignore(t *testing.T, source string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(source, stignoreName))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
