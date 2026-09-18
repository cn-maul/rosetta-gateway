package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"
)

type Config struct {
	Listen      string         `json:"listen"`
	DBPath      string         `json:"db_path"`
	LogLevel    string         `json:"log_level"`
	AdminToken  string         `json:"admin_token"`
	MasterKeyEnv string        `json:"master_key_env"`
	Defaults    Defaults       `json:"defaults"`
	Bootstrap   Bootstrap      `json:"bootstrap"`
}

type Defaults struct {
	UpstreamTimeoutMs    int `json:"upstream_timeout_ms"`
	StreamIdleTimeoutMs  int `json:"stream_idle_timeout_ms"`
	MaxRetries           int `json:"max_retries"`
	MaxRequestBodyBytes  int `json:"max_request_body_bytes"`
}

type Bootstrap struct {
	Providers []BootstrapProvider `json:"providers"`
	Routes    []BootstrapRoute    `json:"routes"`
}

type BootstrapProvider struct {
	Slug        string                `json:"slug"`
	Name        string                `json:"name"`
	Protocol    string                `json:"protocol"`
	Endpoint    string                `json:"endpoint"`
	Credentials []BootstrapCredential `json:"credentials"`
	Models      []string              `json:"models"`
}

type BootstrapCredential struct {
	Label      string `json:"label"`
	APIKeyEnv  string `json:"api_key_env"`
	APIKey     string `json:"api_key"`
}

type BootstrapRoute struct {
	PublicName string `json:"public_name"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9]{2,32}$`)

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.setDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) setDefaults() {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8080"
	}
	if c.DBPath == "" {
		c.DBPath = "./data/gateway.db"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Defaults.UpstreamTimeoutMs == 0 {
		c.Defaults.UpstreamTimeoutMs = 120000
	}
	if c.Defaults.StreamIdleTimeoutMs == 0 {
		c.Defaults.StreamIdleTimeoutMs = 60000
	}
	if c.Defaults.MaxRetries == 0 {
		c.Defaults.MaxRetries = 2
	}
	if c.Defaults.MaxRequestBodyBytes == 0 {
		c.Defaults.MaxRequestBodyBytes = 32 * 1024 * 1024
	}
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return errors.New("listen address is required")
	}
	for i, p := range c.Bootstrap.Providers {
		if !slugRe.MatchString(p.Slug) {
			return fmt.Errorf("bootstrap.providers[%d].slug: must match [a-z0-9]{2,32}, got %q", i, p.Slug)
		}
		if p.Endpoint == "" {
			return fmt.Errorf("bootstrap.providers[%d].endpoint: required", i)
		}
		for j, cred := range p.Credentials {
			if cred.APIKeyEnv == "" && cred.APIKey == "" {
				return fmt.Errorf("bootstrap.providers[%d].credentials[%d]: api_key or api_key_env required", i, j)
			}
		}
	}
	for i, r := range c.Bootstrap.Routes {
		if r.PublicName == "" {
			return fmt.Errorf("bootstrap.routes[%d].public_name: required", i)
		}
		if r.Provider == "" {
			return fmt.Errorf("bootstrap.routes[%d].provider: required", i)
		}
		if r.Model == "" {
			return fmt.Errorf("bootstrap.routes[%d].model: required", i)
		}
	}
	return nil
}

func (c *Config) UpstreamTimeout() time.Duration {
	return time.Duration(c.Defaults.UpstreamTimeoutMs) * time.Millisecond
}

func (c *Config) StreamIdleTimeout() time.Duration {
	return time.Duration(c.Defaults.StreamIdleTimeoutMs) * time.Millisecond
}
