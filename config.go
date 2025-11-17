package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the YAML configuration structure per REQUIREMENTS.md
type Config struct {
	Configuration struct {
		Server struct {
			ListenAddr     string        `yaml:"listenAddr"`
			RequestTimeout time.Duration `yaml:"requestTimeout"`
		} `yaml:"server"`
		Auth struct {
			GitLabBaseURL string        `yaml:"gitlabBaseUrl"`
			AuthCacheTTL  time.Duration `yaml:"authCacheTtl"`
		} `yaml:"auth"`
		Sonatype struct {
			Upstream string        `yaml:"upstream"`
			Username string        `yaml:"username"`
			APIKey   string        `yaml:"apiKey"`
			Timeout  time.Duration `yaml:"timeout"`
		} `yaml:"sonatype"`
		Cache struct {
			Directory  string        `yaml:"directory"`
			ExpireTTL  time.Duration `yaml:"expireTtl"`
			RefreshTTL time.Duration `yaml:"refreshTtl"`
			UnusedTTL  time.Duration `yaml:"unusedTtl"`
			BatchSize  int           `yaml:"batchSize"`
		} `yaml:"cache"`
		Logs struct {
			Journal string `yaml:"journal"`
		} `yaml:"logs"`
	} `yaml:"configuration"`
	log *log.Logger
}

func loadConfig(path string) (*Config, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	applyDefaults(&cfg)
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyDefaults(c *Config) {
	s := &c.Configuration
	if s.Server.ListenAddr == "" {
		s.Server.ListenAddr = ":8080"
	}
	if s.Server.RequestTimeout < time.Second {
		s.Server.RequestTimeout = 30 * time.Second
	}
	if s.Auth.GitLabBaseURL == "" {
		s.Auth.GitLabBaseURL = "https://gitlab.com"
	}
	if s.Auth.AuthCacheTTL < time.Second {
		s.Auth.AuthCacheTTL = 10 * time.Minute
	}
	if s.Sonatype.Upstream == "" {
		s.Sonatype.Upstream = "https://ossindex.sonatype.org/api/v3/component-report"
	}
	if s.Sonatype.Timeout < time.Second {
		s.Sonatype.Timeout = 20 * time.Second
	}
	if s.Cache.UnusedTTL < time.Second {
		s.Cache.UnusedTTL = 120 * time.Hour
	}
	if s.Cache.RefreshTTL < time.Second {
		s.Cache.RefreshTTL = 12 * time.Hour
	}
	if s.Cache.ExpireTTL < time.Second {
		s.Cache.ExpireTTL = 24 * 7 * time.Hour
	}
	if s.Cache.BatchSize < 1 {
		s.Cache.BatchSize = 100
	}
}

func validateConfig(c *Config) error {
	s := &c.Configuration
	if s.Cache.Directory == "" {
		return errors.New("cache.directory must be configured")
	}
	if s.Sonatype.Username == "" || s.Sonatype.APIKey == "" {
		return errors.New("sonatype.username and sonatype.apiKey must be configured")
	}
	return nil
}
