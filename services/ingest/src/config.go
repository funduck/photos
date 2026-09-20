package main

import (
	"fmt"
	"os"
	"os/exec"
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

// Error policies. Each names what happens to the file, not to the service.
const (
	onErrorFallback = "fallback" // copy the original instead of the transcode
	onErrorWarn     = "warn"     // keep going, log it
	onErrorFail     = "fail"     // give up on this file
)

type Config struct {
	Source string `yaml:"source"`
	Dest   string `yaml:"dest"`
	DryRun bool   `yaml:"dry_run"`

	// DestMarker must exist inside Dest before anything is written. An unmounted
	// /Volumes/... leaves behind an empty writable directory; without this the
	// library would end up on the boot disk.
	DestMarker string   `yaml:"dest_marker"`
	DestWait   Duration `yaml:"dest_wait"`

	// DontAsk is set by -dont-ask. Like StopBeforeAction it is a property of
	// this invocation, not of the configuration.
	DontAsk bool `yaml:"-"`

	// StopBeforeAction is set by -stop-before-action, never from the config
	// file: it is a one-off inventory pass, not a mode a daemon should run in.
	StopBeforeAction bool `yaml:"-"`

	Workers          int `yaml:"workers"`
	TranscodeWorkers int `yaml:"transcode_workers"`

	State     StateConfig     `yaml:"state"`
	Scan      ScanConfig      `yaml:"scan"`
	Tools     ToolsConfig     `yaml:"tools"`
	Ignore    IgnoreConfig    `yaml:"ignore"`
	Transcode TranscodeConfig `yaml:"transcode"`
}

type StateConfig struct {
	// Dir is a parent; the DB lands in <Dir>/<slug of Source>/state.db so that
	// several source trees can be ingested without sharing state. Path, if set,
	// overrides the whole computation.
	Dir  string `yaml:"dir"`
	Path string `yaml:"path"`

	AdoptExistingDest bool     `yaml:"adopt_existing_dest"`
	MaxAttempts       int      `yaml:"max_attempts"`
	RetryBackoff      Duration `yaml:"retry_backoff"`
	RetryMax          Duration `yaml:"retry_max"`
}

type ScanConfig struct {
	Interval  Duration `yaml:"interval"`
	Watch     bool     `yaml:"watch"`
	Debounce  Duration `yaml:"debounce"`
	Settle    Duration `yaml:"settle"`
	QueueSize int      `yaml:"queue_size"`
}

type ToolsConfig struct {
	FFmpeg   string   `yaml:"ffmpeg"`
	FFprobe  string   `yaml:"ffprobe"`
	Exiftool string   `yaml:"exiftool"` // empty disables the metadata pass
	Timeout  Duration `yaml:"timeout"`
}

type IgnoreConfig struct {
	Names   []string `yaml:"names"` // globs against the base name, files and dirs
	Paths   []string `yaml:"paths"` // globs against the source-relative path
	MinSize int64    `yaml:"min_size"`
}

type TranscodeConfig struct {
	Enabled          bool   `yaml:"enabled"`
	SidecarPrefix    string `yaml:"sidecar_prefix"`
	KeepIfLarger     bool   `yaml:"keep_if_larger"`
	OnTranscodeError string `yaml:"on_transcode_error"`
	OnMetadataError  string `yaml:"on_metadata_error"`
	Rules            []Rule `yaml:"rules"`
}

type Rule struct {
	Name          string    `yaml:"name"`
	Match         RuleMatch `yaml:"match"`
	FFmpegArgs    []string  `yaml:"ffmpeg_args"`
	ExiftoolArgs  []string  `yaml:"exiftool_args"`
	PreserveMtime bool      `yaml:"preserve_mtime"`
}

type RuleMatch struct {
	Extensions      []string `yaml:"extensions"`
	MinSize         int64    `yaml:"min_size"`
	MinVideoBitrate int64    `yaml:"min_video_bitrate"`
}

func defaultConfig() Config {
	return Config{
		DryRun:           true,
		DestMarker:       ".ingest-dest",
		DestWait:         Duration(30 * time.Second),
		Workers:          2,
		TranscodeWorkers: 1,
		State: StateConfig{
			AdoptExistingDest: true,
			MaxAttempts:       5,
			RetryBackoff:      Duration(5 * time.Minute),
			RetryMax:          Duration(6 * time.Hour),
		},
		Scan: ScanConfig{
			Interval:  Duration(15 * time.Minute),
			Watch:     true,
			Debounce:  Duration(5 * time.Second),
			Settle:    Duration(10 * time.Second),
			QueueSize: 1024,
		},
		Tools: ToolsConfig{
			FFmpeg:   "ffmpeg",
			FFprobe:  "ffprobe",
			Exiftool: "exiftool",
			Timeout:  Duration(2 * time.Hour),
		},
		Ignore: IgnoreConfig{
			Names:   []string{".*", ".ingest.*", "*.tmp", "@eaDir"},
			MinSize: 1,
		},
		Transcode: TranscodeConfig{
			Enabled:          true,
			SidecarPrefix:    ".ingest.",
			OnTranscodeError: onErrorFallback,
			OnMetadataError:  onErrorWarn,
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

func (c *Config) Validate() error {
	for _, p := range []struct{ name, val string }{{"source", c.Source}, {"dest", c.Dest}} {
		if p.val == "" {
			return fmt.Errorf("%s is required", p.name)
		}
		if !filepath.IsAbs(p.val) {
			return fmt.Errorf("%s must be an absolute path, got %q", p.name, p.val)
		}
		fi, err := os.Stat(p.val)
		if err != nil {
			return fmt.Errorf("%s: %w", p.name, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory: %s", p.name, p.val)
		}
	}
	if nested(c.Source, c.Dest) || nested(c.Dest, c.Source) {
		return fmt.Errorf("source and dest must not contain each other (%s, %s)", c.Source, c.Dest)
	}
	if c.DestMarker == "" {
		return fmt.Errorf("dest_marker is required")
	}
	if c.Workers < 1 {
		return fmt.Errorf("workers must be >= 1, got %d", c.Workers)
	}
	if c.TranscodeWorkers < 1 {
		return fmt.Errorf("transcode_workers must be >= 1, got %d", c.TranscodeWorkers)
	}
	if c.Scan.QueueSize < 1 {
		return fmt.Errorf("scan.queue_size must be >= 1, got %d", c.Scan.QueueSize)
	}
	if c.State.MaxAttempts < 1 {
		return fmt.Errorf("state.max_attempts must be >= 1, got %d", c.State.MaxAttempts)
	}
	for _, d := range []struct {
		name string
		val  Duration
	}{
		{"dest_wait", c.DestWait},
		{"scan.settle", c.Scan.Settle},
		{"state.retry_backoff", c.State.RetryBackoff},
		{"state.retry_max", c.State.RetryMax},
		{"tools.timeout", c.Tools.Timeout},
	} {
		if d.val.D() <= 0 {
			return fmt.Errorf("%s must be > 0", d.name)
		}
	}

	if !c.Transcode.Enabled {
		return nil
	}
	if c.Transcode.SidecarPrefix == "" {
		return fmt.Errorf("transcode.sidecar_prefix is required")
	}
	if err := oneOf("transcode.on_transcode_error", c.Transcode.OnTranscodeError, onErrorFallback, onErrorFail); err != nil {
		return err
	}
	if err := oneOf("transcode.on_metadata_error", c.Transcode.OnMetadataError, onErrorWarn, onErrorFail); err != nil {
		return err
	}
	if len(c.Transcode.Rules) == 0 {
		return fmt.Errorf("transcode.enabled is true but no rules are configured")
	}
	for i, r := range c.Transcode.Rules {
		where := fmt.Sprintf("transcode.rules[%d]", i)
		if r.Name != "" {
			where = fmt.Sprintf("transcode rule %q", r.Name)
		}
		if len(r.Match.Extensions) == 0 {
			return fmt.Errorf("%s: match.extensions is required", where)
		}
		if err := checkArgs(where+".ffmpeg_args", r.FFmpegArgs, true); err != nil {
			return err
		}
		if err := checkArgs(where+".exiftool_args", r.ExiftoolArgs, false); err != nil {
			return err
		}
	}

	// Resolve the tools now rather than on the first video, so a launchd PATH
	// problem shows up at startup.
	tools := []string{c.Tools.FFmpeg, c.Tools.FFprobe}
	if c.Tools.Exiftool != "" {
		tools = append(tools, c.Tools.Exiftool)
	}
	for _, t := range tools {
		if _, err := exec.LookPath(t); err != nil {
			return fmt.Errorf("%q not found in PATH: %w", t, err)
		}
	}
	return nil
}

// checkArgs verifies that the {{.In}}/{{.Out}} placeholders appear exactly once.
// Getting this wrong means ffmpeg reads or writes the wrong file, so it is a
// startup error rather than a runtime surprise.
func checkArgs(where string, args []string, required bool) error {
	if len(args) == 0 {
		if required {
			return fmt.Errorf("%s is required", where)
		}
		return nil
	}
	for _, ph := range []string{placeholderIn, placeholderOut} {
		n := 0
		for _, a := range args {
			n += strings.Count(a, ph)
		}
		if n != 1 {
			return fmt.Errorf("%s must contain %s exactly once, found %d", where, ph, n)
		}
	}
	return nil
}

func oneOf(name, val string, allowed ...string) error {
	for _, a := range allowed {
		if val == a {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of %s, got %q", name, strings.Join(allowed, "|"), val)
}

// nested reports whether child is inside parent.
func nested(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
