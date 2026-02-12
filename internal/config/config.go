package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	PollInterval time.Duration `yaml:"-"`
	RawInterval  string        `yaml:"poll_interval"`
	Workdir      string        `yaml:"workdir"`
	LogFile      string        `yaml:"log_file"`
	Claude       ClaudeConfig  `yaml:"claude"`
	Repos        []RepoConfig  `yaml:"repos"`
	Log          LogConfig     `yaml:"log"`
	TUI          TUIConfig     `yaml:"tui"`
}

type ClaudeConfig struct {
	Model string `yaml:"model"`
}

type RepoConfig struct {
	Owner                 string                 `yaml:"owner"`
	Name                  string                 `yaml:"name"`
	BaseBranch            string                 `yaml:"base_branch"`
	ExcludeAuthors        []string               `yaml:"exclude_authors"`
	MergeMethod           string                 `yaml:"merge_method"`
	MaxConcurrentPRs      int                    `yaml:"max_concurrent_prs"`
	RequireCopilotReview  *bool                  `yaml:"require_copilot_review,omitempty"`
	ReviewRequestComment  *ReviewRequestComment  `yaml:"review_request_comment,omitempty"`
}

type ReviewRequestComment struct {
	Enabled bool   `yaml:"enabled"`
	Message string `yaml:"message"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

type TUIConfig struct {
	RefreshInterval time.Duration `yaml:"-"`
	RawInterval     string        `yaml:"refresh_interval"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.setDefaults(); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) setDefaults() error {
	if c.RawInterval == "" {
		c.RawInterval = "60s"
	}
	d, err := time.ParseDuration(c.RawInterval)
	if err != nil {
		return fmt.Errorf("parse poll_interval %q: %w", c.RawInterval, err)
	}
	c.PollInterval = d

	if c.Workdir == "" {
		c.Workdir = "/tmp/auto-claude"
	}
	if c.LogFile == "" {
		c.LogFile = c.Workdir + "/logs/auto-claude.log"
	}
	if c.Claude.Model == "" {
		c.Claude.Model = "opus"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}

	if c.TUI.RawInterval == "" {
		c.TUI.RawInterval = "3s"
	}
	tuiInterval, err := time.ParseDuration(c.TUI.RawInterval)
	if err != nil {
		return fmt.Errorf("parse tui.refresh_interval %q: %w", c.TUI.RawInterval, err)
	}
	if tuiInterval <= 0 {
		return fmt.Errorf("tui.refresh_interval must be positive, got %s", c.TUI.RawInterval)
	}
	c.TUI.RefreshInterval = tuiInterval

	for i := range c.Repos {
		if c.Repos[i].BaseBranch == "" {
			c.Repos[i].BaseBranch = "main"
		}
		if c.Repos[i].MergeMethod == "" {
			c.Repos[i].MergeMethod = "squash"
		}
		if c.Repos[i].MaxConcurrentPRs == 0 {
			c.Repos[i].MaxConcurrentPRs = 3
		}
		if c.Repos[i].RequireCopilotReview == nil {
			defaultTrue := true
			c.Repos[i].RequireCopilotReview = &defaultTrue
		}
	}

	return nil
}

func (c *Config) validate() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("no repos configured")
	}
	for i, r := range c.Repos {
		if r.Owner == "" {
			return fmt.Errorf("repos[%d]: owner required", i)
		}
		if r.Name == "" {
			return fmt.Errorf("repos[%d]: name required", i)
		}
		switch r.MergeMethod {
		case "squash", "merge":
		default:
			return fmt.Errorf("repos[%d]: invalid merge_method %q (squash|merge)", i, r.MergeMethod)
		}
		if r.ReviewRequestComment != nil && r.ReviewRequestComment.Enabled && r.ReviewRequestComment.Message == "" {
			return fmt.Errorf("repos[%d]: review_request_comment.message required when enabled", i)
		}
	}
	return nil
}
