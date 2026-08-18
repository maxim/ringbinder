package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func TestResolveOCRSettings_PrecedenceAndDefaults(t *testing.T) {
	gemini := modelGemini
	mistral := modelMistral
	seven := 7

	tests := []struct {
		name           string
		cfg            *config.Config
		cliModel       string
		cliConcurrency string
		want           ocrSettings
	}{
		{name: "defaults", want: ocrSettings{model: modelMistral, concurrency: 4}},
		{name: "config gemini defaults", cfg: &config.Config{Model: &gemini}, want: ocrSettings{model: modelGemini, concurrency: 20}},
		{name: "config concurrency", cfg: &config.Config{Model: &gemini, OCRConcurrency: &seven}, want: ocrSettings{model: modelGemini, concurrency: 7}},
		{name: "CLI Mistral overrides Gemini", cfg: &config.Config{Model: &gemini, OCRConcurrency: &seven}, cliModel: modelMistral, cliConcurrency: "2", want: ocrSettings{model: modelMistral, concurrency: 2}},
		{name: "CLI Gemini overrides Mistral", cfg: &config.Config{Model: &mistral}, cliModel: modelGemini, want: ocrSettings{model: modelGemini, concurrency: 20}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := ocrSettingsCommand(t, tt.cliModel, tt.cliConcurrency)
			got, err := resolveOCRSettings(cmd, tt.cfg)
			if err != nil {
				t.Fatalf("resolveOCRSettings() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveOCRSettings() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveOCRSettings_RejectsExplicitInvalidValues(t *testing.T) {
	emptyCLI := ocrSettingsCommand(t, "", "")
	if err := emptyCLI.Flags().Set("model", ""); err != nil {
		t.Fatalf("Set(empty model) error = %v", err)
	}
	if _, err := resolveOCRSettings(emptyCLI, nil); err == nil || !strings.Contains(err.Error(), "allowed values are mistral, gemini") {
		t.Fatalf("resolveOCRSettings(empty CLI model) error = %v, want allowed-values error", err)
	}

	tests := []struct {
		name           string
		cfg            *config.Config
		cliModel       string
		cliConcurrency string
		wantError      string
	}{
		{name: "whitespace CLI model", cliModel: " ", wantError: "allowed values are mistral, gemini"},
		{name: "empty config model", cfg: configWithModel(""), wantError: "allowed values are mistral, gemini"},
		{name: "whitespace config model", cfg: configWithModel(" "), wantError: "allowed values are mistral, gemini"},
		{name: "case varied model", cliModel: "Gemini", wantError: "allowed values are mistral, gemini"},
		{name: "versioned model", cliModel: "gemini-3.7-flash", wantError: "allowed values are mistral, gemini"},
		{name: "unknown config model", cfg: configWithModel("other"), wantError: "allowed values are mistral, gemini"},
		{name: "zero CLI concurrency", cliConcurrency: "0", wantError: "--concurrency must be >= 1"},
		{name: "zero config concurrency", cfg: configWithConcurrency(0), wantError: "ocr_concurrency must be >= 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := ocrSettingsCommand(t, tt.cliModel, tt.cliConcurrency)
			_, err := resolveOCRSettings(cmd, tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("resolveOCRSettings() error = %v, want contains %q", err, tt.wantError)
			}
		})
	}
}

func TestNewOCRProvider_RequiresOnlySelectedKey(t *testing.T) {
	t.Run("mistral", func(t *testing.T) {
		t.Setenv("MISTRAL_API_KEY", "mistral-key")
		t.Setenv("GEMINI_API_KEY", "")
		provider, err := newOCRProvider(modelMistral, time.Now())
		if err != nil {
			t.Fatalf("newOCRProvider() error = %v", err)
		}
		if _, ok := provider.(*ocr.MistralClient); !ok {
			t.Fatalf("provider = %T, want MistralClient", provider)
		}
	})

	t.Run("gemini", func(t *testing.T) {
		t.Setenv("MISTRAL_API_KEY", "")
		t.Setenv("GEMINI_API_KEY", "gemini-key")
		provider, err := newOCRProvider(modelGemini, time.Now())
		if err != nil {
			t.Fatalf("newOCRProvider() error = %v", err)
		}
		if _, ok := provider.(*ocr.GeminiClient); !ok {
			t.Fatalf("provider = %T, want GeminiClient", provider)
		}
	})

	t.Run("no fallback", func(t *testing.T) {
		t.Setenv("MISTRAL_API_KEY", "other-provider-key")
		t.Setenv("GEMINI_API_KEY", "")
		if _, err := newOCRProvider(modelGemini, time.Now()); err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
			t.Fatalf("newOCRProvider() error = %v, want selected-key error", err)
		}
	})
}

func TestLoadCommandConfig_ExplicitSettingsAndDatabaseBypassInvalidConfig(t *testing.T) {
	resetCommandState(t)

	invalidConfig := filepath.Join(t.TempDir(), "invalid.yml")
	if err := os.WriteFile(invalidConfig, []byte("paths: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfgFile = invalidConfig

	cmd := commandWithDatabaseFlag(t, filepath.Join(t.TempDir(), "test.db"))
	cmd.Flags().String("model", "", "")
	cmd.Flags().Int("concurrency", 0, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatalf("Set(model) error = %v", err)
	}
	if err := cmd.Flags().Set("concurrency", "3"); err != nil {
		t.Fatalf("Set(concurrency) error = %v", err)
	}

	cfg, err := loadCommandConfig(cmd, "model", "concurrency")
	if err != nil {
		t.Fatalf("loadCommandConfig() error = %v", err)
	}
	if cfg != nil {
		t.Fatalf("loadCommandConfig() = %#v, want nil bypass", cfg)
	}

	cmd.Flag("concurrency").Changed = false
	if _, err := loadCommandConfig(cmd, "model", "concurrency"); err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("loadCommandConfig(missing setting) error = %v, want config error", err)
	}
}

func ocrSettingsCommand(t *testing.T, cliModel, cliConcurrency string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("model", "", "")
	cmd.Flags().Int("concurrency", 0, "")
	if cliModel != "" {
		if err := cmd.Flags().Set("model", cliModel); err != nil {
			t.Fatalf("Set(model) error = %v", err)
		}
	}
	if cliConcurrency != "" {
		if err := cmd.Flags().Set("concurrency", cliConcurrency); err != nil {
			t.Fatalf("Set(concurrency) error = %v", err)
		}
	}
	return cmd
}

func configWithModel(value string) *config.Config {
	return &config.Config{Model: &value}
}

func configWithConcurrency(value int) *config.Config {
	return &config.Config{OCRConcurrency: &value}
}
