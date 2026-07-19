package store_test

import (
	"context"
	"os"
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

func TestStandaloneWriterLockFencesStoreAndIsOwnerOnly(t *testing.T) {
	path := t.TempDir() + "/flowbee.db"
	lock, err := store.AcquireWriterLockForDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	info, err := os.Stat(path + ".writer.lock")
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("writer lock mode=%04o want 0600", got)
	}
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AcquireWriterLock(); err == nil {
		t.Fatal("store acquired writer lock while standalone restore lock was active")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.AcquireWriterLock(); err != nil {
		t.Fatalf("store did not acquire released standalone lock: %v", err)
	}
}
