package worker

import (
	"context"
	"errors"
	"github.com/mgwinsor/streamforge/internal/store"
	"io"
	"log/slog"
	"testing"
	"time"
)

type testRepo struct {
	finished bool
	message  string
}

func (r *testRepo) Claim(context.Context) (store.Video, error) {
	return store.Video{}, store.ErrNotFound
}
func (r *testRepo) Finish(ctx context.Context, id, out, msg string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.finished = true
	r.message = msg
	return nil
}

type canceledProcessor struct{}

func (canceledProcessor) Process(ctx context.Context, id, source string) (string, error) {
	<-ctx.Done()
	return "", errors.New("private diagnostic /data/secret")
}
func TestShutdownRecordsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &testRepo{}
	w := Worker{DB: r, Media: canceledProcessor{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), PollInterval: time.Second}
	w.process(ctx, store.Video{ID: "test"})
	if !r.finished || r.message != "Processing failed; see worker logs." {
		t.Fatalf("failure not recorded: %+v", r)
	}
}
