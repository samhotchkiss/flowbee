package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialEnvelopeRejectsSymlinkAndHardlinkAuthority(t *testing.T) {
	body := []byte(strings.Repeat("s", 64))
	target := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "envelope")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := readOwnerEnvelope(path); err == nil {
		t.Fatal("symlink credential envelope was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := readOwnerEnvelope(path); err == nil {
		t.Fatal("multiply-linked credential envelope was accepted")
	}
}

func TestCredentialEnvelopeReadsValidatedOwnerFile(t *testing.T) {
	want := strings.Repeat("credential", 8)
	path := filepath.Join(t.TempDir(), "envelope")
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readOwnerEnvelope(path)
	if err != nil || string(got) != want {
		t.Fatalf("readOwnerEnvelope() = %q, %v", got, err)
	}
}
