package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func runHuman(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: flowbee human <login-link|bootstrap-link> [flags]")
	}
	if args[0] == "bootstrap-link" {
		return runHumanBootstrapLink(args[1:])
	}
	if args[0] != "login-link" {
		return errors.New("usage: flowbee human <login-link|bootstrap-link> [flags]")
	}
	fs := flag.NewFlagSet("human login-link", flag.ContinueOnError)
	baseURL := fs.String("url", os.Getenv("FLOWBEE_URL"), "Flowbee private/dashboard origin (for example https://flowbee.tailnet.ts.net)")
	projectID := fs.String("project", "default", "project whose explicit human grant authorizes this login")
	token := fs.String("token", os.Getenv("FLOWBEE_WORKER_TOKEN"), "enrolled automation bearer (defaults to FLOWBEE_WORKER_TOKEN)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	path, expires, err := requestHumanLoginLink(context.Background(), &http.Client{Timeout: 10 * time.Second},
		*baseURL, *projectID, *token)
	if err != nil {
		return err
	}
	fmt.Printf("%s%s\nexpires %s\n", strings.TrimRight(*baseURL, "/"), path, expires)
	return nil
}

// runHumanBootstrapLink is the deliberately offline bootstrap for an installation
// that has human dashboard grants but no enrolled automation bearer yet. It may
// only run while the control-plane writer is stopped: the same process-lifetime
// lock used by serve fences the one direct database write. Subsequent links use
// the normal authenticated `human login-link` endpoint.
func runHumanBootstrapLink(args []string) error {
	fs := flag.NewFlagSet("human bootstrap-link", flag.ContinueOnError)
	baseURL := fs.String("url", os.Getenv("FLOWBEE_URL"), "Flowbee private/dashboard origin (for example https://flowbee.tailnet.ts.net)")
	projectID := fs.String("project", "default", "project whose explicit human grant authorizes this login")
	identity := fs.String("identity", "", "exact identity enrolled in FLOWBEE_HUMAN_GRANTS_FILE")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	path, expires, err := bootstrapHumanLoginLink(context.Background(), *baseURL, *projectID, *identity, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Printf("%s%s\nexpires %s\n", strings.TrimRight(*baseURL, "/"), path, expires)
	return nil
}

func bootstrapHumanLoginLink(ctx context.Context, baseURL, projectID, identity string, now time.Time) (string, string, error) {
	return bootstrapHumanLoginLinkChecked(ctx, baseURL, projectID, identity, now, activeControlPlanePID)
}

func bootstrapHumanLoginLinkChecked(ctx context.Context, baseURL, projectID, identity string, now time.Time,
	active func() (int, bool)) (string, string, error) {
	if _, err := humanLoginOrigin(baseURL); err != nil {
		return "", "", err
	}
	projectID, identity = strings.TrimSpace(projectID), strings.TrimSpace(identity)
	if projectID == "" || identity == "" {
		return "", "", errors.New("--project and --identity are required")
	}

	cfg, err := config.Load()
	if err != nil {
		return "", "", err
	}
	// This validates both configured files through O_NOFOLLOW, regular-file,
	// owner-only-mode, size, session-key-length, and grant parsing checks before
	// touching the database.
	access, err := configuredHumanAccess(cfg.PrivateAddr, nil, true)
	if err != nil {
		return "", "", fmt.Errorf("offline human bootstrap: %w", err)
	}
	if err := access.Authorize(auth.HumanPrincipal{Identity: identity}, projectID, auth.HumanDecisionRead); err != nil {
		return "", "", fmt.Errorf("identity %q has no dashboard grant for project %q", identity, projectID)
	}
	if pid, ok := active(); ok {
		return "", "", fmt.Errorf("control plane is active as pid %d; stop it before creating an offline bootstrap link", pid)
	}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return "", "", err
	}
	defer st.Close()
	if err := st.AcquireWriterLock(); err != nil {
		return "", "", fmt.Errorf("offline human bootstrap requires the control-plane writer to be stopped: %w", err)
	}
	// Close the pidfile race around lock acquisition. A correctly configured
	// server cannot overlap because it holds this lock; this also refuses an old
	// serve binary which announced itself but predates the lock invariant.
	if pid, ok := active(); ok {
		return "", "", fmt.Errorf("control plane is active as pid %d; stop it before creating an offline bootstrap link", pid)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		return "", "", err
	}

	rawToken, err := randomHumanBootstrapSecret(32)
	if err != nil {
		return "", "", err
	}
	sessionID, err := randomHumanBootstrapSecret(24)
	if err != nil {
		return "", "", err
	}
	expiresAt := now.UTC().Add(10 * time.Minute)
	if err := st.CreateHumanLoginToken(ctx, rawToken, identity, sessionID, expiresAt, now.UTC()); err != nil {
		return "", "", err
	}
	return "/login#token=" + rawToken, expiresAt.Format(time.RFC3339Nano), nil
}

func activeControlPlanePID() (int, bool) {
	path := pidFilePath()
	if path == "" {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || !isFlowbeeServe(pid) {
		return 0, false
	}
	return pid, true
}

func randomHumanBootstrapSecret(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func requestHumanLoginLink(ctx context.Context, client *http.Client, baseURL, projectID, token string) (string, string, error) {
	baseURL, projectID, token = strings.TrimSpace(baseURL), strings.TrimSpace(projectID), strings.TrimSpace(token)
	if _, err := humanLoginOrigin(baseURL); err != nil {
		return "", "", err
	}
	if projectID == "" || token == "" {
		return "", "", errors.New("--project and --token/FLOWBEE_WORKER_TOKEN are required")
	}
	body, _ := json.Marshal(map[string]string{"project_id": projectID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/v1/human/login-links", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var out struct {
		Path      string `json:"login_fragment_path"`
		ExpiresAt string `json:"expires_at"`
	}
	if resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("login-link request failed: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if !strings.HasPrefix(out.Path, "/login#token=") || out.ExpiresAt == "" {
		return "", "", errors.New("Flowbee returned an invalid login-link response")
	}
	return out.Path, out.ExpiresAt, nil
}

func humanLoginOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.User != nil {
		return nil, errors.New("--url must be an http(s) origin without credentials, path, query, or fragment")
	}
	return u, nil
}
