// Package config loads Flowbee's typed configuration from an optional YAML file
// plus FLOWBEE_* environment overrides, and validates invariants.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DatabaseURL        string `yaml:"database_url"`
	PrivateAddr        string `yaml:"private_addr"`
	HealthAddr         string `yaml:"health_addr"`
	WebhookAddr        string `yaml:"webhook_addr"`
	LeaseTTLS          int    `yaml:"lease_ttl_s"`
	HeartbeatIntervalS int    `yaml:"heartbeat_interval_s"`
	LongPollWaitS      int    `yaml:"long_poll_wait_s"`
	RiverMaxWorkers    int    `yaml:"river_max_workers"`
	LogLevel           string `yaml:"log_level"`
}

func Default() Config {
	return Config{
		DatabaseURL:        "flowbee.db",
		PrivateAddr:        ":7070",
		HealthAddr:         ":7001",
		WebhookAddr:        ":8443",
		LeaseTTLS:          300,
		HeartbeatIntervalS: 30,
		LongPollWaitS:      30,
		RiverMaxWorkers:    10,
		LogLevel:           "info",
	}
}

// Load reads defaults, then flowbee.yaml (or $FLOWBEE_CONFIG), then FLOWBEE_* env
// overrides, then validates.
func Load() (Config, error) {
	c := Default()

	path := os.Getenv("FLOWBEE_CONFIG")
	if path == "" {
		if _, err := os.Stat("flowbee.yaml"); err == nil {
			path = "flowbee.yaml"
		}
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyEnv(&c)
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("FLOWBEE_DATABASE_URL"); v != "" {
		c.DatabaseURL = v
	}
	if v := os.Getenv("FLOWBEE_PRIVATE_ADDR"); v != "" {
		c.PrivateAddr = v
	}
	if v := os.Getenv("FLOWBEE_HEALTH_ADDR"); v != "" {
		c.HealthAddr = v
	}
	if v := os.Getenv("FLOWBEE_WEBHOOK_ADDR"); v != "" {
		c.WebhookAddr = v
	}
	if v := os.Getenv("FLOWBEE_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := envInt("FLOWBEE_LEASE_TTL_S"); v > 0 {
		c.LeaseTTLS = v
	}
	if v := envInt("FLOWBEE_HEARTBEAT_INTERVAL_S"); v > 0 {
		c.HeartbeatIntervalS = v
	}
	if v := envInt("FLOWBEE_LONG_POLL_WAIT_S"); v > 0 {
		c.LongPollWaitS = v
	}
	if v := envInt("FLOWBEE_RIVER_MAX_WORKERS"); v > 0 {
		c.RiverMaxWorkers = v
	}
}

func envInt(key string) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// Validate enforces DESIGN invariants, notably §6.3.3: TTL = k*heartbeat, k>=3.
func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return errors.New("database_url is required")
	}
	if c.HeartbeatIntervalS <= 0 {
		return errors.New("heartbeat_interval_s must be > 0")
	}
	if c.LeaseTTLS < 3*c.HeartbeatIntervalS {
		return fmt.Errorf("lease_ttl_s (%d) must be >= 3*heartbeat_interval_s (%d) per DESIGN §6.3.3",
			c.LeaseTTLS, 3*c.HeartbeatIntervalS)
	}
	return nil
}

func (c Config) LeaseTTL() time.Duration { return time.Duration(c.LeaseTTLS) * time.Second }
func (c Config) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatIntervalS) * time.Second
}
func (c Config) LongPollWait() time.Duration { return time.Duration(c.LongPollWaitS) * time.Second }
