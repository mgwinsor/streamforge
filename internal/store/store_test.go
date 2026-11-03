package store

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentClaim(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL for PostgreSQL integration check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{pool}
	migration, err := os.ReadFile("../../migrations/001_videos.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if _, err = s.Insert(ctx, Video{ID: id, Title: "claim", SourcePath: "/unused"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := s.Claim(ctx)
			if err == nil && v.ID != id {
				err = errors.New("wrong ID")
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins, empty := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrNotFound):
			empty++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || empty != 1 {
		t.Fatalf("claims=%d empty=%d", wins, empty)
	}
	// A pool with a single connection can still query after claim: no retained transaction.
	if err = s.Finish(ctx, id, "/output", ""); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get(ctx, id)
	if err != nil || v.Status != "ready" || v.StartedAt == nil || v.FinishedAt == nil {
		t.Fatalf("bad completion %+v %v", v, err)
	}
}
