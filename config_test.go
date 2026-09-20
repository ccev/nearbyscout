package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func writeTestConfig(t testing.TB, c Config) string {
	t.Helper()
	data, err := toml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigUnknownKeys(t *testing.T) {
	base, err := toml.Marshal(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"", "server", "dragonite", "queue", "filter"} {
		t.Run("section="+section, func(t *testing.T) {
			var values map[string]any
			if err := toml.Unmarshal(base, &values); err != nil {
				t.Fatal(err)
			}
			if section == "" {
				values["unknown"] = true
			} else {
				values[section].(map[string]any)["unknown"] = true
			}
			data, err := toml.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(path); err == nil {
				t.Fatal("accepted unknown config key")
			}
		})
	}
}

func TestLoadConfigInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"", "ftp://localhost/scout/v2", "/scout/v2", "http:///scout/v2",
		"https://:7272/scout/v2", "http://user@localhost/scout/v2",
		"http://user:password@localhost/scout/v2", "http://localhost/scout/v2#fragment",
		"http://[::1/scout/v2",
	} {
		t.Run(endpoint, func(t *testing.T) {
			c := testConfig(t)
			c.Dragonite.Endpoint = endpoint
			if _, err := loadConfig(writeTestConfig(t, c)); err == nil {
				t.Fatalf("accepted endpoint %q", endpoint)
			}
		})
	}
}

func TestLoadConfigInvalidDuration(t *testing.T) {
	for _, field := range []string{"timeout", "dedup_ttl"} {
		for _, value := range []string{"", "bad", "5", "0s", "-1s"} {
			t.Run(field+"="+value, func(t *testing.T) {
				c := testConfig(t)
				if field == "timeout" {
					c.Dragonite.Timeout = value
				} else {
					c.Queue.DedupTTL = value
				}
				if _, err := loadConfig(writeTestConfig(t, c)); err == nil {
					t.Fatalf("accepted %s=%q", field, value)
				}
			})
		}
	}
}

func TestLoadConfigInvalidLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Config, int)
	}{
		{"port", func(c *Config, n int) { c.Server.Port = n }},
		{"max_body_bytes", func(c *Config, n int) { c.Server.MaxBodyBytes = int64(n) }},
		{"max_concurrent_requests", func(c *Config, n int) { c.Server.MaxConcurrentRequests = n }},
		{"workers", func(c *Config, n int) { c.Dragonite.Workers = n }},
		{"batch_size", func(c *Config, n int) { c.Dragonite.BatchSize = n }},
		{"capacity", func(c *Config, n int) { c.Queue.Capacity = n }},
		{"dedup_capacity", func(c *Config, n int) { c.Queue.DedupCapacity = n }},
	} {
		for _, value := range []int{-1, 0} {
			t.Run(fmt.Sprintf("%s=%d", tc.name, value), func(t *testing.T) {
				c := testConfig(t)
				tc.set(&c, value)
				if _, err := loadConfig(writeTestConfig(t, c)); err == nil {
					t.Fatal("accepted nonpositive limit")
				}
			})
		}
	}
	t.Run("port too large", func(t *testing.T) {
		c := testConfig(t)
		c.Server.Port = 65536
		if _, err := loadConfig(writeTestConfig(t, c)); err == nil {
			t.Fatal("accepted port above 65535")
		}
	})
	t.Run("empty expression", func(t *testing.T) {
		c := testConfig(t)
		c.Filter.Expression = ""
		if _, err := loadConfig(writeTestConfig(t, c)); err == nil {
			t.Fatal("accepted empty expression")
		}
	})
}

func TestLoadConfigValid(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1:7272/scout/v2", "https://example.com/scout/v2", "http://[::1]:7272/scout/v2"} {
		for _, port := range []int{1, 65535} {
			t.Run(fmt.Sprintf("%s/port=%d", endpoint, port), func(t *testing.T) {
				c := testConfig(t)
				c.Server.Host = "::1"
				c.Server.Port = port
				c.Dragonite.Endpoint = endpoint
				c.Dragonite.Timeout = "1ms"
				c.Queue.DedupTTL = "1h30m"
				got, err := loadConfig(writeTestConfig(t, c))
				if err != nil {
					t.Fatal(err)
				}
				if got != c {
					t.Fatalf("config = %+v, want %+v", got, c)
				}
			})
		}
	}
}
