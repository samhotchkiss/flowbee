package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/samhotchkiss/flowbee/internal/config"
)

// markerPath returns the pause marker file path that sits beside the live DB.
// pause creates it; resume removes it; serve and status check it.
func markerPath(dbURL string) string {
	return filepath.Join(filepath.Dir(dbURL), "paused")
}

// runPause creates the pause marker so the lease endpoint stops issuing new
// leases (workers go idle after their current job). In-flight leases, heartbeats,
// and result submissions are unaffected. Idempotent.
func runPause(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p := markerPath(cfg.DatabaseURL)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if os.IsExist(err) {
		fmt.Println("fleet already paused")
		return nil
	}
	if err != nil {
		return fmt.Errorf("create pause marker: %w", err)
	}
	f.Close()
	fmt.Printf("fleet PAUSED — no new leases will be issued (marker: %s)\n", p)
	fmt.Println("  in-flight jobs continue; run `flowbee resume` when ready")
	return nil
}

// runResume removes the pause marker so leasing resumes. Idempotent — no error
// if the marker is absent.
func runResume(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p := markerPath(cfg.DatabaseURL)
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("fleet is not paused")
			return nil
		}
		return fmt.Errorf("remove pause marker: %w", err)
	}
	fmt.Println("fleet RESUMED — lease endpoint is open again")
	return nil
}
