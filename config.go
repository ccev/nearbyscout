package main

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Server struct {
		Host                  string `toml:"host"`
		Port                  int    `toml:"port"`
		Token                 string `toml:"token"`
		MaxBodyBytes          int64  `toml:"max_body_bytes"`
		MaxConcurrentRequests int    `toml:"max_concurrent_requests"`
	} `toml:"server"`
	Dragonite struct {
		Endpoint  string `toml:"endpoint"`
		Username  string `toml:"username"`
		Workers   int    `toml:"workers"`
		BatchSize int    `toml:"batch_size"`
		Timeout   string `toml:"timeout"`
	} `toml:"dragonite"`
	Queue struct {
		Capacity      int    `toml:"capacity"`
		DedupCapacity int    `toml:"dedup_capacity"`
		DedupTTL      string `toml:"dedup_ttl"`
	} `toml:"queue"`
	Filter struct {
		Expression string `toml:"expression"`
	} `toml:"filter"`
}

func loadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	if err := toml.NewDecoder(f).DisallowUnknownFields().Decode(&c); err != nil {
		return c, err
	}
	u, err := url.Parse(c.Dragonite.Endpoint)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return c, fmt.Errorf("dragonite.endpoint must be a full HTTP(S) URL without credentials or fragment")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 || c.Server.MaxBodyBytes < 1 || c.Server.MaxConcurrentRequests < 1 || c.Dragonite.Workers < 1 || c.Dragonite.BatchSize < 1 || c.Queue.Capacity < 1 || c.Queue.DedupCapacity < 1 || c.Filter.Expression == "" {
		return c, fmt.Errorf("port must be 1..65535, limits must be positive, and filter.expression must not be empty")
	}
	for name, value := range map[string]string{"dragonite.timeout": c.Dragonite.Timeout, "queue.dedup_ttl": c.Queue.DedupTTL} {
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("%s must be a positive duration", name)
		}
	}
	return c, nil
}
