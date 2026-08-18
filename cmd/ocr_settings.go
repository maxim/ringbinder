package cmd

import (
	"fmt"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/spf13/cobra"
)

const (
	modelMistral = "mistral"
	modelGemini  = "gemini"
)

type ocrSettings struct {
	model       string
	concurrency int
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
	model, err := resolveModel(cmd, cfg)
	if err != nil {
		return ocrSettings{}, err
	}

	concurrency := 0
	if flag := cmd.Flag("concurrency"); flag != nil && flag.Changed {
		concurrency, err = cmd.Flags().GetInt("concurrency")
		if err != nil {
			return ocrSettings{}, fmt.Errorf("read --concurrency flag: %w", err)
		}
		if concurrency < 1 {
			return ocrSettings{}, fmt.Errorf("--concurrency must be >= 1")
		}
	} else if cfg != nil && cfg.OCRConcurrency != nil {
		concurrency = *cfg.OCRConcurrency
		if concurrency < 1 {
			return ocrSettings{}, fmt.Errorf("ocr_concurrency must be >= 1")
		}
	} else if model == modelGemini {
		concurrency = 20
	} else {
		concurrency = 4
	}

	return ocrSettings{model: model, concurrency: concurrency}, nil
}

func resolveModel(cmd *cobra.Command, cfg *config.Config) (string, error) {
	model := ""
	if flag := cmd.Flag("model"); flag != nil && flag.Changed {
		var err error
		model, err = cmd.Flags().GetString("model")
		if err != nil {
			return "", fmt.Errorf("read --model flag: %w", err)
		}
	} else if cfg != nil && cfg.Model != nil {
		model = *cfg.Model
	} else {
		model = modelMistral
	}

	if model != modelMistral && model != modelGemini {
		return "", fmt.Errorf("invalid OCR model %q: allowed values are mistral, gemini", model)
	}
	return model, nil
}
