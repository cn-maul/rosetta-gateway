package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// Default 返回一份可直接启动的最小配置：不含任何 provider/route，
// 全部由管理后台在前端添加。
//
// admin_token 留空**不等于**后台免鉴权：管理凭据独立存放在可执行文件同级的
// admin_auth.json（见 internal/adminauth）。留空且凭据文件不存在时，后台处于
// 「等待首次设置密码」状态 —— 除 password/check 与首次 password/set 外的接口一律 401。
// 这也是默认 listen 只绑回环的原因。
func Default() *Config {
	c := &Config{
		MasterKeyEnv: "ROSETTA_GW_MASTER_KEY",
	}
	c.setDefaults()
	return c
}

// LoadOrGenerate 读取 path 处的配置；文件不存在时写入一份默认配置再返回。
// 第二个返回值 generated 表示本次是否新建了配置文件。
func LoadOrGenerate(path string) (*Config, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c := Default()
		b, mErr := json.MarshalIndent(c, "", "  ")
		if mErr != nil {
			return nil, false, fmt.Errorf("marshal default config: %w", mErr)
		}
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return nil, false, fmt.Errorf("create config dir: %w", mkErr)
			}
		}
		if wErr := os.WriteFile(path, append(b, '\n'), 0o600); wErr != nil {
			return nil, false, fmt.Errorf("write default config: %w", wErr)
		}
		return c, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read config: %w", err)
	}
	c, pErr := Parse(data)
	return c, false, pErr
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
		// 默认只监听回环。网关对外提供 /v1 是常态，但**首次启动时后台还没有任何凭据**，
		// 此时绑 0.0.0.0 等于把「抢先设置管理员密码」的权利交给局域网里第一个访问者。
		// 要对外服务就显式改成 0.0.0.0:<port>（启动日志会打印实际监听地址）。
		c.Listen = "127.0.0.1:8080"
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
