package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("ALBUM_EXPORT_API_KEY", "secret")
	cfg, err := LoadConfig("../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "secret" {
		t.Fatalf("api_key not expanded: %q", cfg.APIKey)
	}
	if !cfg.DryRun {
		t.Fatal("the example must default to dry run")
	}
}

func TestUnknownKeyFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("api_key: x\npoll_intervall: 5m\n"), 0o644)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("typo accepted")
	}
}

func TestValidateJobs(t *testing.T) {
	base := func() Config {
		c := defaultConfig()
		c.APIKey = "k"
		c.Jobs = []Job{
			{Name: "a", Album: "A", Dest: "/export/a"},
			{Name: "b", Album: "B", Dest: "/export/b"},
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
		"nested dest":    {func(c *Config) { c.Jobs[1].Dest = "/export/a/sub" }, "overlapping"},
		"same dest":      {func(c *Config) { c.Jobs[1].Dest = "/export/a/" }, "overlapping"},
		"relative dest":  {func(c *Config) { c.Jobs[0].Dest = "export/a" }, "absolute"},
		"no key":         {func(c *Config) { c.APIKey = "" }, "api_key"},
		"no jobs":        {func(c *Config) { c.Jobs = nil }, "at least one job"},
	} {
		c := base()
		tc.mut(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}

	// Sibling names sharing a prefix are not nested.
	c := base()
	c.Jobs[1].Dest = "/export/ab"
	if err := c.Validate(); err != nil {
		t.Errorf("sibling prefix rejected: %v", err)
	}
}
