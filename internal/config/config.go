package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	DatabaseURL      string
	DataDir          string
	Addr             string
	ProbeTimeout     time.Duration
	TranscodeTimeout time.Duration
	PollInterval     time.Duration
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		DataDir:     env("DATA_DIR", "./data"),
		Addr:        env("HTTP_ADDR", ":8080"),
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}

	for _, v := range []struct {
		name, def string
		dst       *time.Duration
	}{
		{"PROBE_TIMEOUT", "15s", &c.ProbeTimeout},
		{"TRANSCODE_TIMEOUT", "5m", &c.TranscodeTimeout},
		{"POLL_INTERVAL", "1s", &c.PollInterval},
	} {
		d, err := time.ParseDuration(env(v.name, v.def))
		if err != nil || d <= 0 {
			return c, fmt.Errorf("invalid %s", v.name)
		}
		*v.dst = d
	}
	return c, nil
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
