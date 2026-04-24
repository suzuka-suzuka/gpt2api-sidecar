package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig    `yaml:"server"`
	Auth     AuthConfig      `yaml:"auth"`
	Models   []ModelConfig   `yaml:"models"`
	Accounts []AccountConfig `yaml:"accounts"`
}

type ServerConfig struct {
	Listen           string `yaml:"listen"`
	PublicBaseURL    string `yaml:"public_base_url"`
	LogLevel         string `yaml:"log_level"`
	LogFormat        string `yaml:"log_format"`
	RequestTimeout   string `yaml:"request_timeout"`
	QueueWaitTimeout string `yaml:"queue_wait_timeout"`
	MaxQueueSize     int    `yaml:"max_queue_size"`
	AcquireTimeout   string `yaml:"acquire_timeout"`
	MinInterval      string `yaml:"min_interval"`
	Cooldown429      string `yaml:"cooldown_429"`
	BlobTTL          string `yaml:"blob_ttl"`
	MaxImageBytes    int64  `yaml:"max_image_bytes"`
}

type AuthConfig struct {
	APIKeys []string `yaml:"api_keys"`
}

type ModelConfig struct {
	ID       string `yaml:"id"`
	Upstream string `yaml:"upstream"`
	Type     string `yaml:"type"`
}

type AccountConfig struct {
	Name      string `yaml:"name"`
	AuthToken string `yaml:"auth_token"`
	DeviceID  string `yaml:"device_id"`
	SessionID string `yaml:"session_id"`
	ProxyURL  string `yaml:"proxy_url"`
	Cookies   string `yaml:"cookies"`
	Enabled   *bool  `yaml:"enabled,omitempty"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("decode config yaml: %w", err)
	}

	cfg.applyDefaults()
	changed := cfg.ensureStableIDs()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	if changed {
		if err := cfg.save(path); err != nil {
			return nil, err
		}
	}

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:46321"
	}
	if c.Server.PublicBaseURL == "" {
		c.Server.PublicBaseURL = "http://" + c.Server.Listen
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.Server.LogFormat == "" {
		c.Server.LogFormat = "console"
	}
	if c.Server.RequestTimeout == "" {
		c.Server.RequestTimeout = "8m"
	}
	if c.Server.QueueWaitTimeout == "" {
		c.Server.QueueWaitTimeout = "10m"
	}
	if c.Server.MaxQueueSize <= 0 {
		c.Server.MaxQueueSize = 32
	}
	if c.Server.AcquireTimeout == "" {
		c.Server.AcquireTimeout = "2m"
	}
	if c.Server.MinInterval == "" {
		c.Server.MinInterval = "10s"
	}
	if c.Server.Cooldown429 == "" {
		c.Server.Cooldown429 = "5m"
	}
	if c.Server.BlobTTL == "" {
		c.Server.BlobTTL = "20m"
	}
	if c.Server.MaxImageBytes <= 0 {
		c.Server.MaxImageBytes = 16 * 1024 * 1024
	}

	for i := range c.Models {
		if c.Models[i].Upstream == "" {
			c.Models[i].Upstream = "auto"
		}
		if c.Models[i].Type == "" {
			c.Models[i].Type = "image"
		}
	}
}

func (c *Config) validate() error {
	if len(c.Auth.APIKeys) == 0 {
		return errors.New("config.auth.api_keys must contain at least one API key")
	}
	for _, key := range c.Auth.APIKeys {
		if strings.TrimSpace(key) == "" {
			return errors.New("config.auth.api_keys contains an empty item")
		}
	}
	if len(c.Models) == 0 {
		return errors.New("config.models must contain at least one model")
	}
	for _, model := range c.Models {
		if strings.TrimSpace(model.ID) == "" {
			return errors.New("config.models contains a model without id")
		}
		if model.Type != "image" {
			return fmt.Errorf("model %q has unsupported type %q", model.ID, model.Type)
		}
	}
	if len(c.Accounts) == 0 {
		return errors.New("config.accounts must contain at least one account")
	}
	enabledAccounts := 0
	for _, account := range c.Accounts {
		if strings.TrimSpace(account.AuthToken) == "" {
			return fmt.Errorf("account %q is missing auth_token", account.Name)
		}
		if account.Enabled == nil || *account.Enabled {
			enabledAccounts++
		}
	}
	if enabledAccounts == 0 {
		return errors.New("all accounts are disabled")
	}
	if _, err := c.RequestTimeoutDuration(); err != nil {
		return err
	}
	if _, err := c.AcquireTimeoutDuration(); err != nil {
		return err
	}
	if _, err := c.QueueWaitTimeoutDuration(); err != nil {
		return err
	}
	if _, err := c.MinIntervalDuration(); err != nil {
		return err
	}
	if _, err := c.Cooldown429Duration(); err != nil {
		return err
	}
	if _, err := c.BlobTTLDuration(); err != nil {
		return err
	}
	return nil
}

func (c *Config) ensureStableIDs() bool {
	changed := false
	for i := range c.Accounts {
		if strings.TrimSpace(c.Accounts[i].Name) == "" {
			c.Accounts[i].Name = fmt.Sprintf("account-%d", i+1)
			changed = true
		}
		if strings.TrimSpace(c.Accounts[i].DeviceID) == "" {
			c.Accounts[i].DeviceID = uuid.NewString()
			changed = true
		}
		if strings.TrimSpace(c.Accounts[i].SessionID) == "" {
			c.Accounts[i].SessionID = uuid.NewString()
			changed = true
		}
	}
	return changed
}

func (c *Config) save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config yaml: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("persist generated device/session ids: %w", err)
	}
	return nil
}

func (c *Config) RequestTimeoutDuration() (time.Duration, error) {
	return parseDurationField("server.request_timeout", c.Server.RequestTimeout)
}

func (c *Config) AcquireTimeoutDuration() (time.Duration, error) {
	return parseDurationField("server.acquire_timeout", c.Server.AcquireTimeout)
}

func (c *Config) QueueWaitTimeoutDuration() (time.Duration, error) {
	return parseDurationField("server.queue_wait_timeout", c.Server.QueueWaitTimeout)
}

func (c *Config) MinIntervalDuration() (time.Duration, error) {
	return parseDurationField("server.min_interval", c.Server.MinInterval)
}

func (c *Config) Cooldown429Duration() (time.Duration, error) {
	return parseDurationField("server.cooldown_429", c.Server.Cooldown429)
}

func (c *Config) BlobTTLDuration() (time.Duration, error) {
	return parseDurationField("server.blob_ttl", c.Server.BlobTTL)
}

func parseDurationField(fieldName, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s: %w", fieldName, err)
	}
	return d, nil
}
