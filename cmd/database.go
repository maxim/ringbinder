package cmd

import (
	"fmt"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/spf13/cobra"
)

func databaseFlagProvided(cmd *cobra.Command) bool {
	if cmd != nil {
		if flag := cmd.Flag("database"); flag != nil {
			return flag.Changed
		}
	}

	// Some tests call run* helpers directly with lightweight commands that do
	// not inherit root persistent flags. In that case, a non-empty package value
	// still represents an explicit test override.
	return databaseFile != ""
}

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

func openDatabase(cmd *cobra.Command) (*db.DB, error) {
	if databaseFlagProvided(cmd) {
		return openDatabaseWithConfig(cmd, nil)
	}

	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return openDatabaseWithConfig(cmd, cfg)
}

func openDatabaseWithConfig(cmd *cobra.Command, cfg *config.Config) (*db.DB, error) {
	cfgPath := ""
	if cfg != nil {
		cfgPath = cfg.DatabasePath
	}

	dbPath, err := config.ResolveDatabasePath(databaseFile, databaseFlagProvided(cmd), cfgPath)
	if err != nil {
		return nil, err
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return database, nil
}
