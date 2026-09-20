package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes a config file and returns its path, along with the temp
// source/dest directories it points at.
func writeConfig(t *testing.T, body string) (path, src, dst string) {
	t.Helper()
	dir := t.TempDir()
	src = filepath.Join(dir, "src")
	dst = filepath.Join(dir, "dst")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path = filepath.Join(dir, "config.yaml")
	body = strings.ReplaceAll(body, "SRC", src)
	body = strings.ReplaceAll(body, "DST", dst)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, src, dst
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	path, src, dst := writeConfig(t, "source: SRC\ndest: DST\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Source != src || cfg.Dest != dst {
		t.Errorf("paths not read: %q %q", cfg.Source, cfg.Dest)
	}
	if !cfg.DryRun {
		t.Error("dry_run should default to true — this repo's convention for one-way work")
	}
	if cfg.Scan.Interval.D() != 15*time.Minute {
		t.Errorf("scan.interval = %v, want 15m", cfg.Scan.Interval.D())
	}
	if cfg.TranscodeWorkers != 1 {
		t.Errorf("transcode_workers = %d, want 1", cfg.TranscodeWorkers)
	}
	if !cfg.State.AdoptExistingDest {
		t.Error("adopt_existing_dest should default to true")
	}
}

func TestLoadConfigExpandsEnv(t *testing.T) {
	path, _, dst := writeConfig(t, "source: SRC\ndest: ${TEST_STORAGE_DIR}\n")
	t.Setenv("TEST_STORAGE_DIR", dst)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Dest != dst {
		t.Errorf("dest = %q, want %q", cfg.Dest, dst)
	}
}

func TestLoadConfigRejectsUnknownKey(t *testing.T) {
	path, _, _ := writeConfig(t, "source: SRC\ndest: DST\nworkerz: 4\n")
	if _, err := LoadConfig(path); err == nil {
		t.Error("expected an error for an unknown key")
	}
}

func TestLoadConfigRejectsBadDuration(t *testing.T) {
	path, _, _ := writeConfig(t, "source: SRC\ndest: DST\nscan:\n  interval: soon\n")
	if _, err := LoadConfig(path); err == nil {
		t.Error("expected an error for an unparseable duration")
	}
}

// validatableConfig returns a config that passes everything except what a test
// deliberately breaks. Transcoding is off so the tests need no ffmpeg on PATH.
func validatableConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultConfig()
	cfg.Source, cfg.Dest = src, dst
	cfg.Transcode.Enabled = false
	return cfg
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(*Config)
	}{
		{"relative source", func(c *Config) { c.Source = "relative/path" }},
		{"missing dest", func(c *Config) { c.Dest = filepath.Join(c.Dest, "nope") }},
		{"dest inside source", func(c *Config) { c.Dest = c.Source }},
		{"no workers", func(c *Config) { c.Workers = 0 }},
		{"no transcode workers", func(c *Config) { c.TranscodeWorkers = 0 }},
		{"zero settle", func(c *Config) { c.Scan.Settle = 0 }},
		{"no dest marker", func(c *Config) { c.DestMarker = "" }},
		{"enabled with no rules", func(c *Config) { c.Transcode.Enabled = true; c.Transcode.Rules = nil }},
		{"bad on_transcode_error", func(c *Config) {
			c.Transcode.Enabled = true
			c.Transcode.OnTranscodeError = "retry-forever"
			c.Transcode.Rules = []Rule{validRule()}
		}},
		{"bad on_metadata_error", func(c *Config) {
			c.Transcode.Enabled = true
			c.Transcode.OnMetadataError = "explode"
			c.Transcode.Rules = []Rule{validRule()}
		}},
		{"rule without Out placeholder", func(c *Config) {
			c.Transcode.Enabled = true
			r := validRule()
			r.FFmpegArgs = []string{"-i", placeholderIn, "out.mp4"}
			c.Transcode.Rules = []Rule{r}
		}},
		{"rule without extensions", func(c *Config) {
			c.Transcode.Enabled = true
			r := validRule()
			r.Match.Extensions = nil
			c.Transcode.Rules = []Rule{r}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validatableConfig(t)
			tc.break_(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("expected a validation error")
			}
		})
	}

	t.Run("valid", func(t *testing.T) {
		cfg := validatableConfig(t)
		if err := cfg.Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func validRule() Rule {
	return Rule{
		Name:       "video-hevc",
		Match:      RuleMatch{Extensions: []string{".mp4"}},
		FFmpegArgs: []string{"-i", placeholderIn, placeholderOut},
	}
}

// The example config is the thing users copy, so it must actually parse.
func TestExampleConfigParses(t *testing.T) {
	// The example lives at the service root, one level up from the package.
	raw, err := os.ReadFile(filepath.Join("..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("STORAGE_DIR", dir)

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("config.example.yaml does not parse: %v", err)
	}
	if len(cfg.Transcode.Rules) == 0 {
		t.Error("example config has no transcode rules")
	}
	if err := checkArgs("ffmpeg_args", cfg.Transcode.Rules[0].FFmpegArgs, true); err != nil {
		t.Errorf("example ffmpeg_args are wrong: %v", err)
	}
}
