package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStringListAccumulates(t *testing.T) {
	var got stringList
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(os.NewFile(0, os.DevNull))
	fs.Var(&got, "config", "")

	if err := fs.Parse([]string{"-config", "a.yaml", "-config", "b.yaml"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0] != "a.yaml" || got[1] != "b.yaml" {
		t.Errorf("repeated -config = %v, want both paths in order", got)
	}

	var none stringList
	fs2 := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs2.Var(&none, "config", "")
	if err := fs2.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("no -config should leave the list empty so the default applies, got %v", none)
	}
}

func TestLoadConfigNamesJobAfterTheFile(t *testing.T) {
	path, _, _ := writeConfig(t, "source: SRC\ndest: DST\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Name != "config" {
		t.Errorf("name = %q, want the file's base name %q", cfg.Name, "config")
	}

	named, _, _ := writeConfig(t, "name: phones\nsource: SRC\ndest: DST\n")
	cfg, err = LoadConfig(named)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Name != "phones" {
		t.Errorf("an explicit name: should win, got %q", cfg.Name)
	}
}

// jobConfig builds a config that has already "passed" Validate, so the tests
// below exercise only what ValidateJobs adds.
func jobConfig(name, source, dest, stateDir string) Config {
	c := defaultConfig()
	c.Name = name
	c.Source = source
	c.Dest = dest
	c.State.Dir = stateDir
	return c
}

func TestValidateJobs(t *testing.T) {
	ok := []Config{
		jobConfig("phones", "/src/phones", "/library/Phones", "/state"),
		jobConfig("cameras", "/src/cameras", "/library/Cameras", "/state"),
	}
	if err := ValidateJobs(ok); err != nil {
		t.Fatalf("two independent trees should be accepted: %v", err)
	}

	tests := []struct {
		name string
		cfgs []Config
		want string
	}{
		{
			"duplicate name",
			[]Config{
				jobConfig("phones", "/src/a", "/library/A", "/state"),
				jobConfig("phones", "/src/b", "/library/B", "/state"),
			},
			"named",
		},
		{
			"same source",
			[]Config{
				jobConfig("a", "/src/a", "/library/A", "/state"),
				jobConfig("b", "/src/a", "/library/B", "/state"),
			},
			"overlap",
		},
		{
			"nested source",
			[]Config{
				jobConfig("a", "/src/a", "/library/A", "/state"),
				jobConfig("b", "/src/a/inner", "/library/B", "/state"),
			},
			"overlap",
		},
		{
			"source inside another dest",
			[]Config{
				jobConfig("a", "/src/a", "/library/A", "/state"),
				jobConfig("b", "/library/A/inner", "/library/B", "/state"),
			},
			"overlap",
		},
		{
			"shared state database",
			[]Config{
				func() Config {
					c := jobConfig("a", "/src/a", "/library/A", "")
					c.State.Path = "/state/one.db"
					return c
				}(),
				func() Config {
					c := jobConfig("b", "/src/b", "/library/B", "")
					c.State.Path = "/state/one.db"
					return c
				}(),
			},
			"same state database",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateJobs(tc.cfgs)
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSharedEncodeLimiterSerializesJobs is the reason several configs run in
// one process at all: VideoToolbox is a single hardware engine, so two source
// trees must not each believe their transcode_workers: 1 makes them alone.
func TestSharedEncodeLimiterSerializesJobs(t *testing.T) {
	var live, peak int32

	newJobPipeline := func(dir string, encode chan struct{}) (*Pipeline, string, string) {
		src := filepath.Join(dir, "in.mov")
		dst := filepath.Join(dir, "out.mov")
		if err := os.WriteFile(src, make([]byte, 1024), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg := defaultConfig()
		cfg.Source = dir
		cfg.Dest = dir
		cfg.Tools.Exiftool = "" // no metadata pass to stand in for

		out := partPath(cfg.Transcode.SidecarPrefix, dst)
		r := &fakeRunner{onFFmpeg: func([]string) error {
			n := atomic.AddInt32(&live, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
					break
				}
			}
			// Long enough that a second encode would overlap if it could.
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&live, -1)
			// One byte, so the result counts as smaller than the source.
			return os.WriteFile(out, []byte{0}, 0o644)
		}}
		tr := NewTranscoder(cfg.Transcode, cfg.Tools, r, quietLogger())
		return NewPipeline(cfg, nil, nil, tr, encode, quietLogger()), src, dst
	}

	encode := make(chan struct{}, 1)
	rule := &Rule{
		Name:       "hevc",
		Match:      RuleMatch{Extensions: []string{".mov"}},
		FFmpegArgs: []string{"-i", placeholderIn, placeholderOut},
	}

	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		p, src, dst := newJobPipeline(t.TempDir(), encode)
		go func() {
			ok, reason, err := p.encode(context.Background(), rule, src, dst, 1024, 1, time.Time{}, quietLogger())
			if err == nil && !ok {
				t.Errorf("encode declined: %s", reason)
			}
			done <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("encode: %v", err)
		}
	}

	if got := atomic.LoadInt32(&peak); got != 1 {
		t.Errorf("%d encodes ran at once; a limiter of 1 shared by both jobs must allow only 1", got)
	}
}
