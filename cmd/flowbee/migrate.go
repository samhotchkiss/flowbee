package main

import (
	"context"
	"fmt"

	"github.com/samhotchkiss/flowbee/internal/config"
	fbriver "github.com/samhotchkiss/flowbee/internal/river"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runMigrate applies River + Flowbee migrations idempotently. M0 supports `up`.
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

	if err := fbriver.Migrate(ctx, st.Pool); err != nil {
		return err
	}
	if err := store.MigrateUp(ctx, st.Pool); err != nil {
		return err
	}
	fmt.Println("migrations applied")
	return nil
}
