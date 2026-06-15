// Package store is the only I/O seam the deterministic core touches: a pgx pool
// plus hand-written SQL. (Queries/Store interfaces and the atomic claim arrive
// in M1; M0 wires the pool + migrations.)
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool *pgxpool.Pool
}

// Open builds a pgx pool and verifies connectivity.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

func (s *Store) Close() { s.Pool.Close() }
