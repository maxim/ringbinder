package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func TestOCRAndCostModelFlagsAreRegisteredOnlyWhereUsed(t *testing.T) {
	for _, cmd := range []*cobra.Command{ocrCmd, costCmd} {
		flag := cmd.Flags().Lookup("model")
		if flag == nil {
			t.Fatalf("%s --model is not registered", cmd.Name())
		}
		if got := flag.Value.Type(); got != "stringArray" {
			t.Fatalf("%s --model type = %q, want repeatable stringArray", cmd.Name(), got)
		}
	}
	if flag := ocrCmd.Flags().Lookup("concurrency"); flag != nil {
		t.Fatal("ocr --concurrency is still registered")
	}
	if flag := batchCmd.PersistentFlags().Lookup("model"); flag != nil {
		t.Fatal("batch unexpectedly exposes persistent --model")
	}
	batchCommands := append([]*cobra.Command{batchCmd}, batchCmd.Commands()...)
	for _, cmd := range batchCommands {
		if flag := cmd.Flags().Lookup("model"); flag != nil {
			t.Fatalf("%s unexpectedly exposes --model", cmd.CommandPath())
		}
	}
}

func TestResolveOCRSettings_PrecedenceAndDefaults(t *testing.T) {
	configured := []string{modelGemini, modelMistral}
	tests := []struct {
		name string
		cfg  *config.Config
		cli  []string
		want []string
	}{
		{name: "default", want: []string{modelMistral}},
		{name: "config order", cfg: &config.Config{Model: &configured}, want: configured},
		{name: "CLI replaces config", cfg: &config.Config{Model: &configured}, cli: []string{modelMistral}, want: []string{modelMistral}},
		{name: "repeated CLI order", cli: []string{modelGemini, modelMistral}, want: []string{modelGemini, modelMistral}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveOCRSettings(ocrSettingsCommand(t, tt.cli...), tt.cfg)
			if err != nil {
				t.Fatalf("resolveOCRSettings() error = %v", err)
			}
			if fmt.Sprint(got.models) != fmt.Sprint(tt.want) {
				t.Fatalf("models = %v, want %v", got.models, tt.want)
			}
		})
	}
}

func TestResolveOCRSettings_RejectsInvalidModels(t *testing.T) {
	tests := []struct {
		name string
		cli  []string
		cfg  []string
		want string
	}{
		{name: "blank", cli: []string{""}, want: "invalid model"},
		{name: "whitespace", cli: []string{" gemini"}, want: "invalid model"},
		{name: "uppercase", cli: []string{"Gemini"}, want: "invalid model"},
		{name: "unknown", cli: []string{"other"}, want: "invalid model"},
		{name: "comma", cli: []string{"gemini,mistral"}, want: "contains a comma"},
		{name: "duplicate CLI", cli: []string{"gemini", "gemini"}, want: "duplicate model"},
		{name: "empty config", cfg: []string{}, want: "model cannot be empty"},
		{name: "duplicate config", cfg: []string{"mistral", "mistral"}, want: "duplicate model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg *config.Config
			if tt.cfg != nil {
				models := append([]string(nil), tt.cfg...)
				cfg = &config.Config{Model: &models}
			}
			cmd := ocrSettingsCommand(t, tt.cli...)
			if tt.name == "blank" {
				if err := cmd.Flags().Set("model", ""); err != nil {
					t.Fatal(err)
				}
			}
			_, err := resolveOCRSettings(cmd, cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("resolveOCRSettings() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNewOCRProviderChain_EagerlyRequiresEveryKey(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "mistral-key")
	t.Setenv("GEMINI_API_KEY", "")
	if _, err := newOCRProviderChain([]string{modelMistral, modelGemini}, time.Now()); err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("newOCRProviderChain() error = %v, want Gemini key error", err)
	}

	t.Setenv("GEMINI_API_KEY", "gemini-key")
	providers, err := newOCRProviderChain([]string{modelMistral, modelGemini}, time.Now())
	if err != nil {
		t.Fatalf("newOCRProviderChain() error = %v", err)
	}
	if _, ok := providers[modelMistral].provider.(*ocr.MistralClient); !ok {
		t.Fatalf("Mistral provider = %T", providers[modelMistral].provider)
	}
	if _, ok := providers[modelGemini].provider.(*ocr.GeminiClient); !ok {
		t.Fatalf("Gemini provider = %T", providers[modelGemini].provider)
	}
	if got := cap(providers[modelMistral].slots); got != mistralConcurrency {
		t.Fatalf("Mistral slots = %d, want %d", got, mistralConcurrency)
	}
	if got := cap(providers[modelGemini].slots); got != geminiConcurrency {
		t.Fatalf("Gemini slots = %d, want %d", got, geminiConcurrency)
	}
}

func TestRunOCRMissingChainKeyDoesNotCreateDatabase(t *testing.T) {
	resetCommandState(t)
	t.Setenv("MISTRAL_API_KEY", "mistral-key")
	t.Setenv("GEMINI_API_KEY", "")
	databasePath := filepath.Join(t.TempDir(), "must-not-exist.db")
	cmd := commandWithDatabaseFlag(t, databasePath)
	cmd.Flags().StringArray("model", nil, "")
	cmd.Flags().Int("limit", 0, "")
	for _, model := range []string{modelMistral, modelGemini} {
		if err := cmd.Flags().Set("model", model); err != nil {
			t.Fatal(err)
		}
	}
	err := runOCR(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("runOCR() error = %v, want missing Gemini key", err)
	}
	if _, statErr := os.Stat(databasePath); !os.IsNotExist(statErr) {
		t.Fatalf("database stat error = %v, want no database file", statErr)
	}
}

func TestLoadCommandConfig_ExplicitModelAndDatabaseBypassInvalidConfig(t *testing.T) {
	resetCommandState(t)
	invalidConfig := filepath.Join(t.TempDir(), "invalid.yml")
	if err := os.WriteFile(invalidConfig, []byte("paths: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgFile = invalidConfig
	cmd := commandWithDatabaseFlag(t, filepath.Join(t.TempDir(), "test.db"))
	cmd.Flags().StringArray("model", nil, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCommandConfig(cmd, "model")
	if err != nil || cfg != nil {
		t.Fatalf("loadCommandConfig() = %#v, %v; want nil bypass", cfg, err)
	}
	cmd.Flag("model").Changed = false
	if _, err := loadCommandConfig(cmd, "model"); err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("loadCommandConfig() error = %v, want config error", err)
	}
}

func ocrSettingsCommand(t *testing.T, models ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().StringArray("model", nil, "")
	for _, model := range models {
		if err := cmd.Flags().Set("model", model); err != nil {
			t.Fatalf("Set(model) error = %v", err)
		}
	}
	return cmd
}
