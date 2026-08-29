package cmd

import (
	"fmt"
	"strings"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/spf13/cobra"
)

const (
	modelMistral = "mistral"
	modelGemini  = "gemini"
)

var allowedModels = map[string]bool{
	modelMistral: true,
	modelGemini:  true,
}

type ocrSettings struct {
	models []string
}

func loadCommandConfig(cmd *cobra.Command, settingFlags ...string) (*config.Config, error) {
	needsConfig := !databaseFlagProvided(cmd)
	for _, name := range settingFlags {
		flag := cmd.Flag(name)
		if flag == nil || !flag.Changed {
			needsConfig = true
		}
	}
	if !needsConfig {
		return nil, nil
	}
	return loadConfig()
}

func resolveOCRSettings(cmd *cobra.Command, cfg *config.Config) (ocrSettings, error) {
	models := []string{modelMistral}
	if cfg != nil && cfg.Model != nil {
		models = append([]string(nil), (*cfg.Model)...)
	}
	if flag := cmd.Flag("model"); flag != nil && flag.Changed {
		var err error
		models, err = cmd.Flags().GetStringArray("model")
		if err != nil {
			return ocrSettings{}, fmt.Errorf("read --model flag: %w", err)
		}
	}

	if err := validateModels(models); err != nil {
		return ocrSettings{}, err
	}
	return ocrSettings{models: models}, nil
}

func validateModels(models []string) error {
	if len(models) == 0 {
		return fmt.Errorf("model cannot be empty")
	}

	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if strings.Contains(model, ",") {
			return fmt.Errorf("model %q contains a comma; repeat --model for each model", model)
		}
		if model == "" || strings.TrimSpace(model) != model || !allowedModels[model] {
			return fmt.Errorf("invalid model %q: allowed values are mistral, gemini", model)
		}
		if seen[model] {
			return fmt.Errorf("duplicate model %q", model)
		}
		seen[model] = true
	}
	return nil
}
