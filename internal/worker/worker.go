package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mgwinsor/streamforge/internal/media"
	"github.com/mgwinsor/streamforge/internal/store"
)

type Repository interface {
	Claim(context.Context) (store.Video, error)
	Finish(context.Context, string, string, string) error
}

type Processor interface {
	Process(context.Context, string, string) (string, error)
}

type Worker struct {
	DB           Repository
	Media        Processor
	Log          *slog.Logger
	PollInterval time.Duration
}

func (w Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		v, err := w.DB.Claim(ctx)
		if err == nil {
			w.process(ctx, v)
			continue
		}
		if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			w.Log.Error("claim failed", "error", err)
		}
		timer := time.NewTimer(w.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w Worker) process(ctx context.Context, v store.Video) {
	start := time.Now()
	w.Log.Info("processing started", "video_id", v.ID, "status", "processing")
	out, err := w.Media.Process(ctx, v.ID, v.SourcePath)
	msg := ""
	status := "ready"
	if err != nil {
		msg = media.PublicError(err)
		status = "failed"
		w.Log.Error("processing failed", "video_id", v.ID, "status", status, "duration", time.Since(start), "error", err)
	}
	// Shutdown has canceled ctx; use a fresh bounded context to persist the final state.
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if dbErr := w.DB.Finish(finishCtx, v.ID, out, msg); dbErr != nil {
		w.Log.Error("job state update failed; published files may disagree with database; manual recovery required", "video_id", v.ID, "published", out != "", "error", dbErr)
		return
	}
	w.Log.Info("processing finished", "video_id", v.ID, "status", status, "duration", time.Since(start))
}
