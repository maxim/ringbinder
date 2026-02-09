package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Paths []string `yaml:"paths"`
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
		cfg.Paths[i] = expandHome(p)
	}

	return &cfg, nil
}

func expandHome(path string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return path // return unexpanded if home dir unavailable
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
