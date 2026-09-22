package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	APIKey       string   `yaml:"api_key"`
	PollInterval Duration `yaml:"poll_interval"`
	DryRun       bool     `yaml:"dry_run"`

	State StateConfig `yaml:"state"`
	Jobs  []Job       `yaml:"jobs"`
}

type StateConfig struct {
	// Path is one DB for every job; rows are keyed by job name. It lives on a
	// named volume, never in a destination: the destination is synced to a
	// phone and is exactly where files get deleted.
	Path         string   `yaml:"path"`
	MaxAttempts  int      `yaml:"max_attempts"`
	RetryBackoff Duration `yaml:"retry_backoff"`
	RetryMax     Duration `yaml:"retry_max"`
}

// Job exports one album into one directory.
type Job struct {
	// Name keys this job's rows in the state DB and tags its log lines. Renaming
	// a job therefore starts it from scratch and re-exports the whole album.
	Name string `yaml:"name"`
	// Album is an album name, or its UUID. A name must match exactly one album.
	Album string `yaml:"album"`
	Dest  string `yaml:"dest"`
	// LivePhotoVideo also exports the motion half of a live photo, which Immich
	// keeps as a separate hidden asset.
	LivePhotoVideo bool `yaml:"live_photo_video"`
}

func defaultConfig() Config {
	return Config{
		ImmichURL:    "http://immich-server:2283",
		PollInterval: Duration(10 * time.Minute),
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
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	if c.ImmichURL == "" {
		errs = append(errs, errors.New("immich_url is required"))
	}
	if c.APIKey == "" {
		errs = append(errs, errors.New("api_key is required (is ALBUM_EXPORT_API_KEY set?)"))
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
		if j.Album == "" {
			errs = append(errs, fmt.Errorf("%s: album is required", label))
		}
		if !filepath.IsAbs(j.Dest) {
			errs = append(errs, fmt.Errorf("%s: dest must be an absolute path, got %q", label, j.Dest))
		}
	}

	// Two jobs writing into one tree would each see the other's files as name
	// collisions, and the flat part-file sweep of one would race the other.
	for i := range c.Jobs {
		for k := i + 1; k < len(c.Jobs); k++ {
			a, b := filepath.Clean(c.Jobs[i].Dest), filepath.Clean(c.Jobs[k].Dest)
			if within(a, b) || within(b, a) {
				errs = append(errs, fmt.Errorf("jobs %q and %q have overlapping dest %s and %s",
					c.Jobs[i].Name, c.Jobs[k].Name, a, b))
			}
		}
	}
	return errors.Join(errs...)
}

// within reports whether path is dir or somewhere below it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
