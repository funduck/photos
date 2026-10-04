package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("PERSON_ALBUM_API_KEY_OLEG", "secret")
	cfg, err := LoadConfig("../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Jobs[0].APIKey != "secret" {
		t.Fatalf("api_key not expanded: %q", cfg.Jobs[0].APIKey)
	}
	if !cfg.DryRun {
		t.Fatal("the example must default to dry run")
	}
}

func TestUnknownKeyFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("poll_intervall: 5m\n"), 0o644)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("typo accepted")
	}
}

func TestMatchDefaultsToAny(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("jobs:\n  - {name: a, api_key: x, people: [Max], album: A}\n"), 0o644)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Jobs[0].Match; got != MatchAny {
		t.Fatalf("match %q, want %q", got, MatchAny)
	}
}

func TestValidateJobs(t *testing.T) {
	base := func() Config {
		c := defaultConfig()
		c.Jobs = []Job{
			{Name: "a", APIKey: "k1", People: []string{"Max"}, Match: MatchAny, Album: "A"},
			{Name: "b", APIKey: "k2", People: []string{"Max", "Kate"}, Match: MatchAll, Album: "B"},
		}
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		mut  func(*Config)
		want string
	}{
		"duplicate name": {func(c *Config) { c.Jobs[1].Name = "a" }, "used twice"},
		"no people":      {func(c *Config) { c.Jobs[0].People = nil }, "people is required"},
		"empty person":   {func(c *Config) { c.Jobs[0].People = []string{""} }, "empty entry"},
		"bad match":      {func(c *Config) { c.Jobs[0].Match = "some" }, "match must be"},
		"no album":       {func(c *Config) { c.Jobs[0].Album = "" }, "album is required"},
		"no key":         {func(c *Config) { c.Jobs[1].APIKey = "" }, `job "b": api_key`},
		"no jobs":        {func(c *Config) { c.Jobs = nil }, "at least one job"},
	} {
		c := base()
		tc.mut(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}
