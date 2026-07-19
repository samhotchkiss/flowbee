package store_test

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestControlPlaneWriterLockFencesOverlappingProcesses(t *testing.T) {
	path := t.TempDir() + "/flowbee.db"
	first, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	if err := second.AcquireWriterLock(); err == nil {
		t.Fatal("second control-plane writer acquired the same database")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.AcquireWriterLock(); err != nil {
		t.Fatalf("writer lock not released on close: %v", err)
	}
}
