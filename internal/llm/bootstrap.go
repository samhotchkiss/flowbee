package llm

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

var bootstrapDefault struct {
	mu sync.Mutex
	st *store.Store
}

// EnsureDefaultAgentRouter installs a process-local router backed by the configured
// Flowbee database. It intentionally resolves the same persistent
// model_slot_binding rows that operators update; a slot swap must be a database
// row update, not a private in-memory seed or code deploy.
func EnsureDefaultAgentRouter(ctx context.Context) error {
	if getDefaultRouter() != nil {
		return nil
	}
	bootstrapDefault.mu.Lock()
	defer bootstrapDefault.mu.Unlock()
	if getDefaultRouter() != nil {
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if isEphemeralDatabaseURL(cfg.DatabaseURL) {
		return fmt.Errorf("llm router db %q is not persistent; model_slot_binding swaps must use the configured Flowbee database", cfg.DatabaseURL)
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open llm router db %q: %w", cfg.DatabaseURL, err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		_ = st.Close()
		return fmt.Errorf("migrate llm router db %q: %w", cfg.DatabaseURL, err)
	}
	bootstrapDefault.st = st
	SetDefaultRouter(NewRouter(st.DB))
	return nil
}

// UseDatabaseAsDefaultRouter lets the control-plane process share its already
// opened, migrated store connection with the router instead of opening a second
// handle. Provider construction remains private to internal/llm.
func UseDatabaseAsDefaultRouter(db *sql.DB) {
	SetDefaultRouter(NewRouter(db))
}

func isEphemeralDatabaseURL(dsn string) bool {
	dsn = strings.ToLower(strings.TrimSpace(dsn))
	return dsn == ":memory:" || strings.Contains(dsn, "mode=memory")
}
