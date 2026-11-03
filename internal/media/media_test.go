package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, audio bool) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	p := filepath.Join(t.TempDir(), "sample.mp4")
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30"}
	if audio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000")
	}
	args = append(args, "-t", "5", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	if audio {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, p)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	return p
}
func TestProcess(t *testing.T) {
	for _, audio := range []bool{false, true} {
		t.Run(map[bool]string{false: "silent", true: "audio"}[audio], func(t *testing.T) {
			p := Processor{Root: t.TempDir(), ProbeTimeout: 10 * time.Second, TranscodeTimeout: time.Minute}
			out, err := p.Process(context.Background(), "test", fixture(t, audio))
			if err != nil {
				t.Fatal(err)
			}
			if err = Verify(out); err != nil {
				t.Fatal(err)
			}
			for _, r := range []string{"360p", "720p"} {
				b, err := exec.Command("ffmpeg", "-v", "error", "-i", filepath.Join(out, r, "index.m3u8"), "-f", "null", "-").CombinedOutput()
				if err != nil {
					t.Fatalf("decode: %v %s", err, b)
				}
			}
		})
	}
}
func TestCorrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(p, []byte("garbage"), 0600)
	proc := Processor{Root: t.TempDir(), ProbeTimeout: time.Second, TranscodeTimeout: time.Second}
	_, err := proc.Process(context.Background(), "bad", p)
	if err == nil {
		t.Fatal("accepted corrupt input")
	}
	entries, _ := os.ReadDir(proc.Root)
	if len(entries) != 0 {
		t.Fatal("left output")
	}
}
func TestCancellation(t *testing.T) {
	source := fixture(t, false)
	p := Processor{Root: t.TempDir(), ProbeTimeout: 10 * time.Second, TranscodeTimeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.Process(ctx, "cancel", source); done <- err }()
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("finished before cancellation: %v", err)
		case <-deadline:
			t.Fatal("transcode never started")
		case <-ticker.C:
			matches, _ := filepath.Glob(filepath.Join(p.Root, ".cancel-*", "360p", "segment*.ts"))
			if len(matches) > 0 {
				cancel()
				goto canceled
			}
		}
	}
canceled:
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel hung")
	}
	entries, _ := os.ReadDir(p.Root)
	if len(entries) != 0 {
		t.Fatalf("left output: %v", entries)
	}
}
func TestVerifyRejectsMissingSegment(t *testing.T) {
	dir := t.TempDir()
	for _, r := range []string{"360p", "720p"} {
		os.Mkdir(filepath.Join(dir, r), 0755)
		os.WriteFile(filepath.Join(dir, r, "index.m3u8"), []byte("#EXTM3U\nmissing.ts\n#EXT-X-ENDLIST\n"), 0600)
	}
	os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte("#EXTM3U\n360p/index.m3u8\n720p/index.m3u8\n"), 0600)
	if err := Verify(dir); err == nil || !strings.Contains(err.Error(), "segment") {
		t.Fatalf("got %v", err)
	}
}
