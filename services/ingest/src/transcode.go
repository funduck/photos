package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

const (
	placeholderIn  = "{{.In}}"
	placeholderOut = "{{.Out}}"
)

// Stages, as recorded in the .failed sidecar.
const (
	stageTranscode = "transcode"
	stageMetadata  = "metadata"
	stageCopy      = "copy"
)

// errMetadata marks an exiftool failure so the caller can apply
// on_metadata_error rather than treating it as a failed encode.
var errMetadata = errors.New("metadata")

type Transcoder struct {
	cfg    TranscodeConfig
	tools  ToolsConfig
	runner Runner
	log    *slog.Logger
}

func NewTranscoder(cfg TranscodeConfig, tools ToolsConfig, r Runner, log *slog.Logger) *Transcoder {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Transcoder{cfg: cfg, tools: tools, runner: r, log: log}
}

// MatchRule returns the first rule whose extension and size conditions match.
// The bitrate condition is deliberately not checked here: probing costs a
// subprocess, so it only happens once a rule has claimed the file.
func (t *Transcoder) MatchRule(name string, size int64) *Rule {
	if !t.cfg.Enabled {
		return nil
	}
	ext := strings.ToLower(extOf(name))
	for i := range t.cfg.Rules {
		r := &t.cfg.Rules[i]
		if size < r.Match.MinSize {
			continue
		}
		for _, e := range r.Match.Extensions {
			if strings.ToLower(e) == ext {
				return r
			}
		}
	}
	return nil
}

// ProbeBitrate returns the video stream's bitrate, falling back to the container
// bitrate. Anything unparseable is reported as 0, which reads as "below any
// threshold" and sends the file down the plain-copy path — the same forgiving
// behaviour as scripts/transcode-videos.sh.
func (t *Transcoder) ProbeBitrate(ctx context.Context, path string) int64 {
	for _, q := range []struct {
		what string
		args []string
	}{
		{"stream", []string{"-v", "error", "-select_streams", "v:0", "-show_entries", "stream=bit_rate", "-of", "default=nw=1:nk=1", path}},
		{"format", []string{"-v", "error", "-show_entries", "format=bit_rate", "-of", "default=nw=1:nk=1", path}},
	} {
		res, err := t.runner.Run(ctx, t.tools.FFprobe, q.args)
		if err != nil {
			// Silently treating this as "no bitrate" is how a broken ffprobe
			// turns into a library quietly copied through untranscoded.
			t.log.Warn("ffprobe failed", "query", q.what, "path", path, "err", err)
			continue
		}
		br := parseBitrate(res.Stdout)
		t.log.Debug("probed bitrate", "query", q.what, "path", path,
			"raw", strings.TrimSpace(res.Stdout), "bitrate", br)
		if br > 0 {
			return br
		}
	}
	return 0
}

// parseBitrate reads the first line of ffprobe output, treating "N/A" and
// anything non-numeric as unknown.
func parseBitrate(out string) int64 {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// Transcode encodes src into out per the rule, then copies the original's tags
// across. A non-nil error wrapping errMetadata means the encode itself
// succeeded but exiftool did not.
func (t *Transcoder) Transcode(ctx context.Context, r *Rule, src, out string) error {
	if _, err := t.runner.Run(ctx, t.tools.FFmpeg, renderArgs(r.FFmpegArgs, src, out)); err != nil {
		return err
	}
	if t.tools.Exiftool == "" || len(r.ExiftoolArgs) == 0 {
		return nil
	}
	// Every tag except Rotation: ffmpeg has already rotated the pixels, and
	// copying the tag back would rotate the video a second time.
	if _, err := t.runner.Run(ctx, t.tools.Exiftool, renderArgs(r.ExiftoolArgs, src, out)); err != nil {
		return fmt.Errorf("%w: %w", errMetadata, err)
	}
	return nil
}

// renderArgs substitutes the {{.In}} / {{.Out}} placeholders. Each argv entry is
// substituted whole, so paths with spaces need no quoting.
func renderArgs(args []string, in, out string) []string {
	rendered := make([]string, len(args))
	for i, a := range args {
		a = strings.ReplaceAll(a, placeholderIn, in)
		rendered[i] = strings.ReplaceAll(a, placeholderOut, out)
	}
	return rendered
}

// extOf returns the extension including the dot, or "" if there is none.
func extOf(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}
