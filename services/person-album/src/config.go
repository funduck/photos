package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that reads from YAML as "15m", "2h", "30s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

type Config struct {
	ImmichURL    string   `yaml:"immich_url"`
	PollInterval Duration `yaml:"poll_interval"`
	DryRun       bool     `yaml:"dry_run"`

	State StateConfig `yaml:"state"`
	Jobs  []Job       `yaml:"jobs"`
}

type StateConfig struct {
	// Path is one DB for every job; rows are keyed by job name.
	Path         string   `yaml:"path"`
	MaxAttempts  int      `yaml:"max_attempts"`
	RetryBackoff Duration `yaml:"retry_backoff"`
	RetryMax     Duration `yaml:"retry_max"`
}

// Match values: whether a photo needs any one of a job's people, or all of them.
const (
	MatchAny = "any"
	MatchAll = "all"
)

// Job adds the photos of some people to one album.
type Job struct {
	// Name keys this job's rows in the state DB and tags its log lines. Renaming
	// a job therefore starts it from scratch and adds every match again.
	Name string `yaml:"name"`
	// APIKey is per job because people and albums are per Immich user: the key
	// decides whose faces are searched and whose albums are seen.
	APIKey string `yaml:"api_key"`
	// People are person names, or their UUIDs. A name must match exactly one person.
	People []string `yaml:"people"`
	Match  string   `yaml:"match"`
	// Album is an album name, or its UUID. A name must match exactly one album.
	Album string `yaml:"album"`
}

func defaultConfig() Config {
	return Config{
		ImmichURL:    "http://immich-server:2283",
		PollInterval: Duration(15 * time.Minute),
		DryRun:       true,
		State: StateConfig{
			Path:         "/state/state.db",
			MaxAttempts:  5,
			RetryBackoff: Duration(time.Minute),
			RetryMax:     Duration(6 * time.Hour),
		},
	}
}

// LoadConfig reads path, expands ${VAR} from the environment, and decodes it over
// the defaults. Unknown keys are an error: a typo in a config is worth failing on.
func LoadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	dec := yaml.NewDecoder(strings.NewReader(os.ExpandEnv(string(raw))))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	for i := range cfg.Jobs {
		if cfg.Jobs[i].Match == "" {
			cfg.Jobs[i].Match = MatchAny
		}
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	if c.ImmichURL == "" {
		errs = append(errs, errors.New("immich_url is required"))
	}
	if c.PollInterval.D() <= 0 {
		errs = append(errs, errors.New("poll_interval must be positive"))
	}
	if c.State.Path == "" {
		errs = append(errs, errors.New("state.path is required"))
	}
	if c.State.MaxAttempts < 1 {
		errs = append(errs, errors.New("state.max_attempts must be at least 1"))
	}
	if len(c.Jobs) == 0 {
		errs = append(errs, errors.New("at least one job is required"))
	}

	names := map[string]bool{}
	for i, j := range c.Jobs {
		label := fmt.Sprintf("jobs[%d]", i)
		if j.Name != "" {
			label = fmt.Sprintf("job %q", j.Name)
		}
		switch {
		case j.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required", label))
		case names[j.Name]:
			errs = append(errs, fmt.Errorf("%s: name is used twice", label))
		}
		names[j.Name] = true
		if j.APIKey == "" {
			errs = append(errs, fmt.Errorf("%s: api_key is required (is its variable set in .env and passed in docker-compose.yml?)", label))
		}
		if len(j.People) == 0 {
			errs = append(errs, fmt.Errorf("%s: people is required", label))
		}
		for _, p := range j.People {
			if p == "" {
				errs = append(errs, fmt.Errorf("%s: people has an empty entry", label))
			}
		}
		if j.Match != MatchAny && j.Match != MatchAll {
			errs = append(errs, fmt.Errorf("%s: match must be %q or %q, got %q", label, MatchAny, MatchAll, j.Match))
		}
		if j.Album == "" {
			errs = append(errs, fmt.Errorf("%s: album is required", label))
		}
	}
	return errors.Join(errs...)
}
