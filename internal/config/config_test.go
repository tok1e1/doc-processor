package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.WorkerConcurrency != 8 || c.JobTimeout != 30*time.Second {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("WORKER_CONCURRENCY", "16")
	t.Setenv("JOB_TIMEOUT", "5s")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkerConcurrency != 16 || c.JobTimeout != 5*time.Second {
		t.Fatalf("env not applied: %+v", c)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := map[string]string{
		"WORKER_CONCURRENCY": "0",
		"MAX_ATTEMPTS":       "abc",
		"JOB_TIMEOUT":        "soon",
		"POSTGRES_MAX_CONNS": "100000",
	}
	for key, value := range tests {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%s must be rejected", key, value)
			}
		})
	}
}
