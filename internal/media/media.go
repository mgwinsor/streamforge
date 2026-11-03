// Package media validates short MP4 inputs and publishes complete HLS output.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Processor struct {
	Root                           string
	ProbeTimeout, TranscodeTimeout time.Duration
}
type Failure struct {
	Message string
	Cause   error
}

func (e *Failure) Error() string { return e.Message + ": " + e.Cause.Error() }
func (e *Failure) Unwrap() error { return e.Cause }

func PublicError(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Message
	}
	return "Processing failed; see worker logs."
}
func fail(msg string, err error) error { return &Failure{msg, err} }

// cappedBuffer retains only a bounded prefix while continuing to drain the pipe.
type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if left := b.limit - b.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		b.Buffer.Write(p)
	}
	return n, nil
}

func run(ctx context.Context, bin string, args []string, limit int) ([]byte, error) {
	out := &cappedBuffer{limit: limit}
	diag := &cappedBuffer{limit: 16 << 10}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = out
	cmd.Stderr = diag
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", bin, err, diag.String())
	}
	return out.Bytes(), nil
}

type probeResult struct {
	Format struct {
		Duration string `json:"duration"`
		Name     string `json:"format_name"`
	} `json:"format"`
	Streams []struct {
		Type          string `json:"codec_type"`
		Width, Height int
		SAR           string `json:"sample_aspect_ratio"`
		Tags          map[string]string
		SideData      []struct {
			Rotation float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
}

func (p Processor) inspect(ctx context.Context, source string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.ProbeTimeout)
	defer cancel()
	b, err := run(ctx, "ffprobe", []string{"-v", "error", "-protocol_whitelist", "file,pipe", "-show_format", "-show_streams", "-of", "json", source}, 1<<20)
	if err != nil {
		return false, fail("Input is not a readable MP4 video.", err)
	}
	var info probeResult
	if err = json.Unmarshal(b, &info); err != nil {
		return false, fail("Unable to inspect input.", err)
	}
	d, err := strconv.ParseFloat(info.Format.Duration, 64)
	if err != nil || math.IsNaN(d) || math.IsInf(d, 0) || d <= 0 || d > 60 {
		return false, fail("Clip duration must be greater than zero and at most 60 seconds.", fmt.Errorf("duration %q", info.Format.Duration))
	}
	if !strings.Contains(info.Format.Name, "mp4") {
		return false, fail("Input must be an MP4 container.", errors.New("unsupported container"))
	}
	audio, video := false, false
	for _, s := range info.Streams {
		if s.Type == "audio" {
			audio = true
		}
		if s.Type != "video" || video {
			continue
		}
		video = true
		if s.Height < 720 || s.Width < s.Height || s.Width > 4096 || s.Height > 2160 || (s.SAR != "" && s.SAR != "N/A" && s.SAR != "1:1") {
			return false, fail("Use a landscape, square-pixel video from 720p to 4K.", errors.New("unsupported dimensions or pixel aspect ratio"))
		}
		for _, sd := range s.SideData {
			if sd.Rotation != 0 {
				return false, fail("Rotated video is unsupported; export a landscape MP4.", errors.New("rotation metadata"))
			}
		}
		if r := s.Tags["rotate"]; r != "" && r != "0" {
			return false, fail("Rotated video is unsupported; export a landscape MP4.", errors.New("rotation tag"))
		}
	}
	if !video {
		return false, fail("Input has no video stream.", errors.New("no video"))
	}
	return audio, nil
}

func (p Processor) Process(ctx context.Context, id, source string) (string, error) {
	audio, err := p.inspect(ctx, source)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(p.Root, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(p.Root, "."+id+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, p.TranscodeTimeout)
	defer cancel()
	master := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n"
	for _, r := range []struct{ name, height, rate, bandwidth string }{{"360p", "360", "800k", "1100000"}, {"720p", "720", "2800k", "3500000"}} {
		dir := filepath.Join(tmp, r.name)
		if err = os.Mkdir(dir, 0o755); err != nil {
			return "", err
		}
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-threads", "2", "-protocol_whitelist", "file,pipe", "-noautorotate", "-i", source, "-map", "0:v:0"}
		if audio {
			args = append(args, "-map", "0:a:0", "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "48000")
		} else {
			args = append(args, "-an")
		}
		args = append(args, "-vf", "scale=-2:"+r.height+",setsar=1,fps=30", "-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-profile:v", "main", "-pix_fmt", "yuv420p", "-b:v", r.rate, "-maxrate", r.rate, "-bufsize", "5600k", "-g", "120", "-keyint_min", "120", "-sc_threshold", "0", "-flags", "+cgop", "-force_key_frames", "expr:gte(t,n_forced*4)", "-t", "60", "-f", "hls", "-hls_time", "4", "-hls_playlist_type", "vod", "-hls_flags", "independent_segments", "-hls_segment_type", "mpegts", "-hls_segment_filename", filepath.Join(dir, "segment%04d.ts"), filepath.Join(dir, "index.m3u8"))
		if _, err = run(ctx, "ffmpeg", args, 1024); err != nil {
			return "", fail("Transcoding failed or was interrupted.", err)
		}
		master += "#EXT-X-STREAM-INF:BANDWIDTH=" + r.bandwidth + "\n" + r.name + "/index.m3u8\n"
	}
	if err = os.WriteFile(filepath.Join(tmp, "master.m3u8"), []byte(master), 0o644); err != nil {
		return "", err
	}
	if err = Verify(tmp); err != nil {
		return "", fail("Generated output failed validation.", err)
	}
	if err = ctx.Err(); err != nil {
		return "", fail("Transcoding was interrupted.", err)
	}
	final := filepath.Join(p.Root, id)
	// A stopped-worker manual requeue may leave a previously published directory.
	if _, err = os.Stat(final); err == nil {
		return "", fail("Output already exists; follow manual recovery instructions.", errors.New("publication conflict"))
	}
	if err = os.Rename(tmp, final); err != nil {
		return "", err
	}
	return final, nil
}

// Verify checks the fixed rendition set and every local segment reference before publication.
func Verify(root string) error {
	b, err := os.ReadFile(filepath.Join(root, "master.m3u8"))
	if err != nil {
		return err
	}
	master := string(b)
	for _, r := range []string{"360p", "720p"} {
		if !strings.Contains(master, r+"/index.m3u8") {
			return errors.New("master missing rendition")
		}
		dir := filepath.Join(root, r)
		b, err = os.ReadFile(filepath.Join(dir, "index.m3u8"))
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(b), "#EXTM3U") || !strings.Contains(string(b), "#EXT-X-ENDLIST") {
			return errors.New("incomplete VOD playlist")
		}
		count := 0
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if filepath.Base(line) != line || !strings.HasSuffix(line, ".ts") {
				return errors.New("invalid segment reference")
			}
			st, err := os.Stat(filepath.Join(dir, line))
			if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
				return fmt.Errorf("missing or empty segment %s", line)
			}
			count++
		}
		if count == 0 {
			return errors.New("no segments")
		}
	}
	return nil
}
