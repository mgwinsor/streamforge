// Package store persists videos and atomically claims queued work.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("video not found")

type Video struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	SourcePath     string     `json:"-"`
	OutputPath     string     `json:"-"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	FailureMessage string     `json:"failure_message,omitempty"`
	PlaybackURL    string     `json:"playback_url,omitempty"`
}

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	c, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}

	c.MaxConns = 5
	p, err := pgxpool.NewWithConfig(ctx, c)
	if err != nil {
		return nil, err
	}

	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}

	return &Store{p}, nil
}

func (s *Store) Close()                         { s.Pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

const columns = `id::text,title,source_path,output_path,status,created_at,started_at,finished_at,failure_message`

func scan(row pgx.Row) (Video, error) {
	var v Video
	err := row.Scan(&v.ID, &v.Title, &v.SourcePath, &v.OutputPath, &v.Status, &v.CreatedAt, &v.StartedAt, &v.FinishedAt, &v.FailureMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if v.Status == "ready" {
		v.PlaybackURL = "/media/" + v.ID + "/master.m3u8"
	}
	return v, err
}

func (s *Store) Insert(ctx context.Context, v Video) (Video, error) {
	return scan(s.Pool.QueryRow(ctx, `INSERT INTO videos(id,title,source_path) VALUES($1,$2,$3) RETURNING `+columns, v.ID, v.Title, v.SourcePath))
}

func (s *Store) Get(ctx context.Context, id string) (Video, error) {
	return scan(s.Pool.QueryRow(ctx, `SELECT `+columns+` FROM videos WHERE id=$1`, id))
}

func (s *Store) List(ctx context.Context) ([]Video, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+columns+` FROM videos ORDER BY created_at DESC,id DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vs := make([]Video, 0)
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v)
	}
	return vs, rows.Err()
}

func (s *Store) Claim(ctx context.Context) (Video, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Video{}, err
	}
	defer tx.Rollback(context.Background())
	v, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM videos WHERE status='queued' ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`))
	if err != nil {
		return Video{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE videos SET status='processing',started_at=now(),finished_at=NULL,failure_message='' WHERE id=$1`, v.ID)
	if err != nil {
		return Video{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Video{}, err
	}
	v.Status = "processing"
	return v, nil
}

func (s *Store) Finish(ctx context.Context, id, path, message string) error {
	status := "ready"
	if message != "" {
		status = "failed"
		path = ""
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE videos SET status=$2,output_path=$3,failure_message=$4,finished_at=now() WHERE id=$1 AND status='processing'`, id, status, path, message)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("finish %s: processing row missing", id)
	}
	return err
}

func (s *Store) Requeue(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE videos SET status='queued',started_at=NULL,finished_at=NULL,output_path='',failure_message='' WHERE id=$1 AND status='processing'`, id)
	if err == nil && tag.RowsAffected() != 1 {
		return errors.New("only an interrupted processing job can be requeued")
	}
	return err
}
