CREATE TABLE IF NOT EXISTS videos (
    id uuid PRIMARY KEY,
    title text NOT NULL,
    source_path text NOT NULL,
    output_path text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','processing','ready','failed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    failure_message text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS videos_queue ON videos(created_at, id) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS videos_recent ON videos(created_at DESC, id DESC);
