package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/mgwinsor/streamforge/internal/config"
	"github.com/mgwinsor/streamforge/internal/media"
	"github.com/mgwinsor/streamforge/internal/store"
	"github.com/mgwinsor/streamforge/internal/worker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	requeue := flag.String("requeue", "", "requeue interrupted ID; ALL workers must be stopped")
	flag.Parse()

	c, err := config.Load()
	if err != nil {
		return err
	}

	root, err := filepath.Abs(c.DataDir)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startup, cancel := context.WithTimeout(ctx, 10*time.Second)

	db, err := store.Open(startup, c.DatabaseURL)
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()

	if *requeue != "" {
		id, err := uuid.Parse(*requeue)
		if err != nil {
			return err
		}

		v, err := db.Get(ctx, id.String())
		if err != nil {
			return err
		}
		if v.Status != "processing" {
			return fmt.Errorf("only processing jobs can be requeued")
		}
		if err = os.RemoveAll(filepath.Join(root, "output", id.String())); err != nil {
			return err
		}

		matches, err := filepath.Glob(filepath.Join(root, "output", "."+id.String()+"-*"))
		if err != nil {
			return err
		}

		for _, path := range matches {
			if err = os.RemoveAll(path); err != nil {
				return err
			}
		}

		if err = db.Requeue(ctx, id.String()); err != nil {
			return err
		}

		log.Info("job requeued", "video_id", id.String())
		return nil
	}
	w := worker.Worker{
		DB: db,
		Media: media.Processor{
			Root:             filepath.Join(root, "output"),
			ProbeTimeout:     c.ProbeTimeout,
			TranscodeTimeout: c.TranscodeTimeout,
		},
		Log:          log,
		PollInterval: c.PollInterval,
	}

	w.Run(ctx)
	return nil
}
