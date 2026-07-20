package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type originStub string

func (o originStub) ExactOrigin(context.Context, string) (string, error) { return string(o), nil }

func TestProjectMarkerCanonicalizesOriginAndPersistsPrivateLocalMarker(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "checkout")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	infoDir := filepath.Join(base, "git-data", "info")
	resolver := FileProjectInitResolver{RepoRoot: alias, GitInfoDir: infoDir,
		RequestedProjectID: "russ", Origins: originStub("git@GitHub.com:Sam/Russ.git")}
	init, err := resolver.ResolveProjectInit(context.Background(), "russ")
	if err != nil {
		t.Fatal(err)
	}
	if init.CWD != canonicalRoot || init.RepositoryOrigin != "github.com/sam/russ" {
		t.Fatalf("init = %+v", init)
	}
	markerPath := filepath.Join(canonicalRoot, ".flowbee", "project.json")
	info, err := os.Stat(markerPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode = %v, %v", info, err)
	}
	exclude, err := os.ReadFile(filepath.Join(infoDir, "exclude"))
	if err != nil || !strings.Contains(string(exclude), projectMarkerIgnore) {
		t.Fatalf("local exclude = %q, %v", exclude, err)
	}
	// HTTPS and SSH spellings resolve to the same stable identity and do not
	// rewrite or duplicate the marker/exclude entry.
	resolver.Origins = originStub("https://github.com/sam/russ.git")
	if _, err := resolver.ResolveProjectInit(context.Background(), "russ"); err != nil {
		t.Fatal(err)
	}
	exclude, _ = os.ReadFile(filepath.Join(infoDir, "exclude"))
	if strings.Count(string(exclude), projectMarkerIgnore) != 1 {
		t.Fatalf("duplicate local exclude entry: %q", exclude)
	}
}

func TestProjectMarkerRejectsCredentialedFirstOriginAndUsesMarkerFirstOnRerun(t *testing.T) {
	root := t.TempDir()
	infoDir := filepath.Join(t.TempDir(), "info")
	resolver := FileProjectInitResolver{RepoRoot: root, GitInfoDir: infoDir,
		RequestedProjectID: "russ", Origins: originStub("https://token@github.com/sam/russ.git")}
	if _, err := resolver.ResolveProjectInit(context.Background(), "russ"); err == nil {
		t.Fatal("credentialed origin was accepted")
	}
	resolver.Origins = originStub("https://github.com/sam/russ.git")
	if _, err := resolver.ResolveProjectInit(context.Background(), "russ"); err != nil {
		t.Fatal(err)
	}
	resolver.Origins = originStub("https://github.com/sam/different.git")
	init, err := resolver.ResolveProjectInit(context.Background(), "russ")
	if err != nil || init.RepositoryOrigin != "github.com/sam/russ" {
		t.Fatalf("marker-first rerun=%+v err=%v", init, err)
	}
}

func TestProjectMarkerRejectsSymlinkAndHardlinkAuthority(t *testing.T) {
	root := t.TempDir()
	markerDir := filepath.Join(root, ".flowbee")
	if err := os.Mkdir(markerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"format":"flowbee.project-marker/v1","project_id":"russ","repository_origin":"github.com/sam/russ","repository_path":"` + root + `"}`)
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(markerDir, "project.json")
	if err := os.Symlink(target, markerPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readProjectMarker(markerPath); err == nil {
		t.Fatal("symlink marker was accepted")
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, markerPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readProjectMarker(markerPath); err == nil {
		t.Fatal("multiply-linked marker was accepted")
	}
}
