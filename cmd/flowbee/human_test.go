package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestRequestHumanLoginLinkUsesAuthenticatedServerBootstrap(t *testing.T) {
	var gotAuth bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Method == http.MethodPost && r.URL.Path == "/v1/human/login-links" &&
			r.Header.Get("Authorization") == "Bearer enrolled-token"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"login_fragment_path":"/login#token=raw-once","expires_at":"2026-07-19T18:10:00Z"}`))
	}))
	defer ts.Close()
	path, expires, err := requestHumanLoginLink(t.Context(), ts.Client(), ts.URL, "default", "enrolled-token")
	if err != nil || !gotAuth || path != "/login#token=raw-once" || expires == "" {
		t.Fatalf("path=%q expires=%q auth=%v err=%v", path, expires, gotAuth, err)
	}
}

func TestRequestHumanLoginLinkRejectsUnsafeOrigin(t *testing.T) {
	if _, _, err := requestHumanLoginLink(t.Context(), http.DefaultClient,
		"https://flowbee.example/dashboard?token=leak", "default", "x"); err == nil {
		t.Fatal("accepted URL with query")
	}
}

func configureHumanBootstrapTest(t *testing.T, grant string) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flowbee.db")
	configPath := filepath.Join(dir, "flowbee.yaml")
	if err := os.WriteFile(configPath, []byte("database_url: "+dbPath+"\nprivate_addr: ':7070'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := writeOwnerFile(t, "human-session.key", "01234567890123456789012345678901\n")
	grants := writeOwnerFile(t, "human-grants", grant+"\n")
	t.Setenv("FLOWBEE_CONFIG", configPath)
	t.Setenv("FLOWBEE_DATABASE_URL", "")
	t.Setenv("FLOWBEE_HUMAN_SESSION_KEY_FILE", key)
	t.Setenv("FLOWBEE_HUMAN_GRANTS_FILE", grants)
	t.Setenv("FLOWBEE_HUMAN_LOOPBACK_DEV", "")
	return dbPath
}

func noActiveControlPlane() (int, bool) { return 0, false }

func TestOfflineHumanBootstrapStoresOnlyOneTimeDigest(t *testing.T) {
	dbPath := configureHumanBootstrapTest(t, "sam@*=admin")
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	path, expires, err := bootstrapHumanLoginLinkChecked(t.Context(), "https://flowbee.example.ts.net",
		"default", "sam", now, noActiveControlPlane)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, "/login#token=") || expires != "2026-07-19T18:10:00Z" {
		t.Fatalf("path=%q expires=%q", path, expires)
	}
	raw := strings.TrimPrefix(path, "/login#token=")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var digest, identity, state string
	if err := st.DB.QueryRow(`SELECT token_sha256,identity,state FROM human_login_tokens`).Scan(&digest, &identity, &state); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(digest, raw) || !strings.HasPrefix(digest, "sha256:") || identity != "sam" || state != "pending" {
		t.Fatalf("digest=%q identity=%q state=%q", digest, identity, state)
	}
	login, err := st.ConsumeHumanLoginToken(t.Context(), raw, now.Add(time.Minute))
	if err != nil || login.Identity != "sam" {
		t.Fatalf("login=%+v err=%v", login, err)
	}
	if _, err := st.ConsumeHumanLoginToken(t.Context(), raw, now.Add(2*time.Minute)); err != store.ErrHumanLoginUsed {
		t.Fatalf("replay err=%v", err)
	}
}

func TestOfflineHumanBootstrapFailsClosedOnWriterLock(t *testing.T) {
	dbPath := configureHumanBootstrapTest(t, "sam@default=viewer")
	first, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	_, _, err = bootstrapHumanLoginLinkChecked(t.Context(), "https://flowbee.example.ts.net",
		"default", "sam", time.Now(), noActiveControlPlane)
	if err == nil || !strings.Contains(err.Error(), "writer to be stopped") {
		t.Fatalf("err=%v", err)
	}
}

func TestOfflineHumanBootstrapFailsClosedForActiveServerUnsafeFilesAndWrongGrant(t *testing.T) {
	configureHumanBootstrapTest(t, "sam@other=admin")
	_, _, err := bootstrapHumanLoginLinkChecked(t.Context(), "https://flowbee.example.ts.net",
		"default", "sam", time.Now(), noActiveControlPlane)
	if err == nil || !strings.Contains(err.Error(), "no dashboard grant") {
		t.Fatalf("wrong-grant err=%v", err)
	}

	grants := os.Getenv("FLOWBEE_HUMAN_GRANTS_FILE")
	if err := os.Chmod(grants, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = bootstrapHumanLoginLinkChecked(t.Context(), "https://flowbee.example.ts.net",
		"other", "sam", time.Now(), noActiveControlPlane)
	if err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("unsafe-file err=%v", err)
	}

	if err := os.Chmod(grants, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = bootstrapHumanLoginLinkChecked(t.Context(), "https://flowbee.example.ts.net",
		"other", "sam", time.Now(), func() (int, bool) { return 4242, true })
	if err == nil || !strings.Contains(err.Error(), "active as pid 4242") {
		t.Fatalf("active-server err=%v", err)
	}
}
