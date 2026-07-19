package main

import (
	"context"
	"fmt"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runMigrate applies migrations idempotently. M0 supports `up`.
func runMigrate(args []string) error {
	dir := "up"
	if len(args) > 0 {
		dir = args[0]
	}
	if dir != "up" {
		return fmt.Errorf("only `migrate up` is supported in M0 (got %q)", dir)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.AcquireWriterLock(); err != nil {
		return fmt.Errorf("migration requires the control-plane writer to be stopped: %w", err)
	}

	snapshot, err := migrateWithRollbackSnapshot(ctx, st.DB,
		envOr("FLOWBEE_BACKUP_DIR", defaultBackupDir()))
	if err != nil {
		return err
	}
	if snapshot != "" {
		fmt.Printf("pre-migration snapshot verified: %s\n", snapshot)
	}
	fmt.Println("migrations applied")
	return nil
}
