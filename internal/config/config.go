package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Paths            []string `yaml:"paths"`
	DatabasePath     string   `yaml:"database_path"`
	Model            *string  `yaml:"model"`
	SweepConcurrency *int     `yaml:"sweep_concurrency"`
	OCRConcurrency   *int     `yaml:"ocr_concurrency"`
}

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	type plainConfig Config
	var decoded plainConfig
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = Config(decoded)

	// yaml.v3 maps a present null scalar to a nil pointer just like an absent
	// key. Preserve key presence so command validation rejects explicit blanks
	// rather than silently selecting provider defaults.
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "model":
			if c.Model == nil {
				empty := ""
				c.Model = &empty
			}
		case "sweep_concurrency":
			if c.SweepConcurrency == nil {
				zero := 0
				c.SweepConcurrency = &zero
			}
		case "ocr_concurrency":
			if c.OCRConcurrency == nil {
				zero := 0
				c.OCRConcurrency = &zero
			}
		}
	}
	return nil
}

func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Fall back to current directory as last resort
		fmt.Fprintf(os.Stderr, "warning: could not determine home directory: %v\n", err)
		return filepath.Join(".", ".config", "ringbinder")
	}
	return filepath.Join(home, ".config", "ringbinder")
}

func DefaultPath() string {
	return filepath.Join(DefaultDir(), "config.yml")
}

func DefaultDatabasePath() string {
	return filepath.Join(DefaultDir(), "ringbinder.db")
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Expand ~ in paths
	for i, p := range cfg.Paths {
		cfg.Paths[i] = ExpandHome(p)
	}

	cfg.DatabasePath = strings.TrimSpace(cfg.DatabasePath)
	if cfg.DatabasePath != "" {
		cfg.DatabasePath = ExpandHome(cfg.DatabasePath)
	}

	return &cfg, nil
}

func ExpandHome(path string) string {
	if path != "~" && !(len(path) >= 2 && path[:2] == "~/") {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path // return unexpanded if home dir unavailable
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

func ResolveDatabasePath(cliPath string, cliProvided bool, cfgPath string) (string, error) {
	if cliProvided {
		path := strings.TrimSpace(cliPath)
		if path == "" {
			return "", fmt.Errorf("--database cannot be empty")
		}
		return ExpandHome(path), nil
	}

	path := strings.TrimSpace(cfgPath)
	if path != "" {
		return ExpandHome(path), nil
	}

	return DefaultDatabasePath(), nil
}
