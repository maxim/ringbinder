package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Paths            []string  `yaml:"paths"`
	DatabasePath     string    `yaml:"database_path"`
	Model            *[]string `yaml:"-"`
	Exclude          []string  `yaml:"-"`
	SweepConcurrency *int      `yaml:"sweep_concurrency"`
}

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	var decoded struct {
		Paths            []string `yaml:"paths"`
		DatabasePath     string   `yaml:"database_path"`
		SweepConcurrency *int     `yaml:"sweep_concurrency"`
	}
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = Config{
		Paths:            decoded.Paths,
		DatabasePath:     decoded.DatabasePath,
		SweepConcurrency: decoded.SweepConcurrency,
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "model":
			models, err := decodeStringOrList(value, "model", false)
			if err != nil {
				return err
			}
			c.Model = &models
		case "exclude":
			patterns, err := decodeStringOrList(value, "exclude", true)
			if err != nil {
				return err
			}
			c.Exclude = patterns
		case "ocr_concurrency":
			return fmt.Errorf("ocr_concurrency is no longer supported; OCR provider limits are fixed")
		case "sweep_concurrency":
			// yaml.v3 maps a present null scalar to a nil pointer just like an
			// absent key. Preserve presence so command validation rejects null.
			if c.SweepConcurrency == nil {
				zero := 0
				c.SweepConcurrency = &zero
			}
		}
	}
	return nil
}

func decodeStringOrList(node *yaml.Node, setting string, allowEmpty bool) ([]string, error) {
	if node.Tag == "!!null" {
		return nil, fmt.Errorf("%s cannot be null", setting)
	}

	var values []string
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return nil, fmt.Errorf("%s must be a string or an array of strings", setting)
		}
		values = []string{node.Value}
	case yaml.SequenceNode:
		if len(node.Content) == 0 && !allowEmpty {
			return nil, fmt.Errorf("%s cannot be empty", setting)
		}
		values = make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
				return nil, fmt.Errorf("%s entries must be strings", setting)
			}
			values = append(values, item.Value)
		}
	default:
		return nil, fmt.Errorf("%s must be a string or an array of strings", setting)
	}

	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s entries cannot be blank", setting)
		}
	}
	return values, nil
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
