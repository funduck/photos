package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeRunner stands in for ffmpeg/ffprobe/exiftool so the decision logic can be
// tested on a machine that has none of them.
type fakeRunner struct {
	stdout map[string]string // by binary name
	fail   map[string]error  // by binary name
	calls  []string
	// onFFmpeg, when set, produces the output file ffmpeg would have written.
	onFFmpeg func(args []string) error
}

func (f *fakeRunner) Run(_ context.Context, name string, args []string) (RunResult, error) {
	f.calls = append(f.calls, name)
	if err, ok := f.fail[name]; ok {
		return RunResult{Stderr: "boom"}, err
	}
	if name == "ffmpeg" && f.onFFmpeg != nil {
		if err := f.onFFmpeg(args); err != nil {
			return RunResult{}, err
		}
	}
	return RunResult{Stdout: f.stdout[name]}, nil
}

func TestParseBitrate(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"N/A", 0},
		{"N/A\n", 0},
		{"12345\n", 12345},
		{"  12345  \n", 12345},
		{"12345\n67890\n", 12345}, // first line wins
		{"abc", 0},
	}
	for _, tc := range tests {
		if got := parseBitrate(tc.in); got != tc.want {
			t.Errorf("parseBitrate(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestProbeBitrateFallsBackToContainer(t *testing.T) {
	// The stream query returns nothing usable, so the container bitrate is used.
	r := &fakeRunner{stdout: map[string]string{"ffprobe": "N/A"}}
	tr := NewTranscoder(defaultConfig().Transcode, defaultConfig().Tools, r, nil)

	if got := tr.ProbeBitrate(context.Background(), "x.mp4"); got != 0 {
		t.Errorf("unparseable bitrate should read as 0, got %d", got)
	}
	if len(r.calls) != 2 {
		t.Errorf("expected a fallback probe, got %d calls", len(r.calls))
	}
}

func TestMatchRule(t *testing.T) {
	cfg := defaultConfig().Transcode
	cfg.Rules = []Rule{{
		Name:       "video-hevc",
		Match:      RuleMatch{Extensions: []string{".mp4", ".mov"}, MinSize: 10},
		FFmpegArgs: []string{"-i", placeholderIn, placeholderOut},
	}}
	tr := NewTranscoder(cfg, defaultConfig().Tools, &fakeRunner{}, nil)

	if tr.MatchRule("VID.MP4", 100) == nil {
		t.Error("extension matching should ignore case")
	}
	if tr.MatchRule("VID.mov", 100) == nil {
		t.Error(".mov should match")
	}
	if tr.MatchRule("IMG.jpg", 100) != nil {
		t.Error("an image should not match the video rule")
	}
	if tr.MatchRule("VID.mp4", 5) != nil {
		t.Error("a file below min_size should not match")
	}

	cfg.Enabled = false
	off := NewTranscoder(cfg, defaultConfig().Tools, &fakeRunner{}, nil)
	if off.MatchRule("VID.mp4", 100) != nil {
		t.Error("no rule should match when transcoding is disabled")
	}
}

func TestRenderArgs(t *testing.T) {
	got := renderArgs([]string{"-i", placeholderIn, "-c:v", "hevc", placeholderOut}, "in file.mp4", "out.mp4")
	want := []string{"-i", "in file.mp4", "-c:v", "hevc", "out.mp4"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("renderArgs = %q, want %q", got, want)
		}
	}
}

func testRule() Rule {
	return Rule{
		Name:         "video-hevc",
		Match:        RuleMatch{Extensions: []string{".mp4"}},
		FFmpegArgs:   []string{"-i", placeholderIn, placeholderOut},
		ExiftoolArgs: []string{"-TagsFromFile", placeholderIn, placeholderOut},
	}
}

// An exiftool failure is distinguishable from an encode failure, because the
// two get different policies (on_metadata_error vs on_transcode_error).
func TestTranscodeTagsMetadataFailures(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mp4")
	out := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &fakeRunner{
		fail:     map[string]error{"exiftool": errors.New("no write permission")},
		onFFmpeg: func([]string) error { return os.WriteFile(out, []byte("x"), 0o644) },
	}
	tr := NewTranscoder(defaultConfig().Transcode, defaultConfig().Tools, r, nil)

	err := tr.Transcode(context.Background(), ptr(testRule()), src, out)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, errMetadata) {
		t.Errorf("exiftool failure should be tagged as a metadata error, got %v", err)
	}
}

func TestTranscodeFFmpegFailureIsNotMetadata(t *testing.T) {
	r := &fakeRunner{fail: map[string]error{"ffmpeg": errors.New("encoder not found")}}
	tr := NewTranscoder(defaultConfig().Transcode, defaultConfig().Tools, r, nil)

	err := tr.Transcode(context.Background(), ptr(testRule()), "in.mp4", "out.mp4")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, errMetadata) {
		t.Error("an encode failure must not be reported as a metadata failure")
	}
}

func TestTranscodeSkipsExiftoolWhenUnset(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")

	r := &fakeRunner{onFFmpeg: func([]string) error { return os.WriteFile(out, []byte("x"), 0o644) }}
	tools := defaultConfig().Tools
	tools.Exiftool = ""
	tr := NewTranscoder(defaultConfig().Transcode, tools, r, nil)

	if err := tr.Transcode(context.Background(), ptr(testRule()), "in.mp4", out); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if c == "exiftool" {
			t.Error("exiftool should not run when it is not configured")
		}
	}
}

func ptr[T any](v T) *T { return &v }

// The reason a file was copied instead of transcoded must survive to the log
// and the state DB: "no-rule" for everything made a library that came through
// untranscoded impossible to debug.
func TestDecideReasons(t *testing.T) {
	cfg := defaultConfig()
	cfg.Transcode.Rules = []Rule{{
		Name:       "video-hevc",
		Match:      RuleMatch{Extensions: []string{".mp4"}, MinVideoBitrate: 8_000_000},
		FFmpegArgs: []string{"-i", placeholderIn, placeholderOut},
	}}

	tests := []struct {
		name       string
		file       string
		probe      string
		disable    bool
		wantRule   bool
		wantReason string
	}{
		{name: "high bitrate transcodes", file: "VID.mp4", probe: "17000000\n", wantRule: true},
		{name: "low bitrate", file: "VID.mp4", probe: "4000000\n", wantReason: "low-bitrate"},
		{name: "unknown bitrate", file: "VID.mp4", probe: "N/A\n", wantReason: "bitrate-unknown"},
		{name: "not a video", file: "IMG.jpg", wantReason: "no-rule"},
		{name: "transcoding off", file: "VID.mp4", disable: true, wantReason: "transcode-disabled"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			c.Transcode.Enabled = !tc.disable
			r := &fakeRunner{stdout: map[string]string{"ffprobe": tc.probe}}
			p := &Pipeline{
				cfg: c,
				tr:  NewTranscoder(c.Transcode, c.Tools, r, nil),
				log: quietLogger(),
			}

			rule, _, reason := p.decide(context.Background(), "/src/"+tc.file, 100, quietLogger())
			if tc.wantRule {
				if rule == nil {
					t.Fatalf("expected a transcode, got reason %q", reason)
				}
				return
			}
			if rule != nil {
				t.Fatal("expected no transcode")
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
