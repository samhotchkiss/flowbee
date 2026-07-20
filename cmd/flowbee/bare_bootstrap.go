package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/bootstrap"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/driver"
)

var errBareBootstrapControlAPI = errors.New("bare bootstrap requires a server-owned authenticated bootstrap-intent API for actor lifecycle, seats, and human attach")

const (
	bareBootstrapTimeoutEnv     = "FLOWBEE_BOOTSTRAP_TIMEOUT"
	bareBootstrapDefaultTimeout = 5 * time.Minute
	bareBootstrapMinTimeout     = 30 * time.Second
	bareBootstrapMaxTimeout     = 30 * time.Minute
	bareBootstrapPollTimeout    = 5 * time.Second
)

type bareBootstrapPreflight struct {
	RepoRoot, GitInfoDir, Origin string
	ProjectID, RepositoryID      string
}

// runBareBootstrap is the no-argument product path. It performs every available
// read-only proof before reporting a missing mutation surface. It never opens or
// writes flowbee.db: once serve is live, all product mutations must go through an
// authenticated server-owned API. The separate bootstrap ledger is opened only
// after a complete executable plan exists.
func runBareBootstrap(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: flowbee")
	}
	timeout, err := bareBootstrapOverallTimeout()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return executeBareBootstrap(ctx, productionBareBootstrapSystem{})
}

func bareBootstrapOverallTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(bareBootstrapTimeoutEnv))
	if raw == "" {
		return bareBootstrapDefaultTimeout, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout < bareBootstrapMinTimeout || timeout > bareBootstrapMaxTimeout {
		return 0, fmt.Errorf("%s must be a duration between %s and %s", bareBootstrapTimeoutEnv,
			bareBootstrapMinTimeout, bareBootstrapMaxTimeout)
	}
	return timeout, nil
}

type bareBootstrapSystem interface {
	Config() (config.Config, error)
	Git(context.Context, ...string) (string, error)
	DriverInventory() (config.DriverEndpointInventory, bool, error)
	DriverServiceEnsurer(config.DriverEndpoint) (bootstrap.DriverServiceEnsurer, error)
	ProbeDrivers(context.Context, config.DriverEndpointInventory) error
	ControlPlaneReady(context.Context, config.Config) (bool, error)
	ControlPlaneBootstrapReady(context.Context, config.Config, string) (bool, error)
	EnsureControlPlane(context.Context, config.DriverEndpointInventory, bareControlPlaneSpec, string) (driver.LifecycleReceipt, error)
}

type productionBareBootstrapSystem struct{}

func (productionBareBootstrapSystem) Config() (config.Config, error) { return config.Load() }
func (productionBareBootstrapSystem) DriverInventory() (config.DriverEndpointInventory, bool, error) {
	return config.LoadDriverEndpointInventoryFromEnv()
}
func (productionBareBootstrapSystem) DriverServiceEnsurer(endpoint config.DriverEndpoint) (bootstrap.DriverServiceEnsurer, error) {
	if endpoint.ServiceEnsure == nil {
		return nil, errors.New("Driver endpoint has no pinned service Ensure authority")
	}
	return pinnedDriverServiceEnsurer{ManagerPath: endpoint.ServiceEnsure.ServiceManagerPath,
		ManagerSHA256: endpoint.ServiceEnsure.ServiceManagerSHA256}, nil
}
func (productionBareBootstrapSystem) ProbeDrivers(ctx context.Context, inventory config.DriverEndpointInventory) error {
	_, _, err := loadServeDriverEndpoints(ctx, inventory)
	return err
}
func (productionBareBootstrapSystem) ControlPlaneReady(ctx context.Context, cfg config.Config) (bool, error) {
	return probeBareControlPlaneReadiness(ctx, cfg.HealthAddr, "/healthz", "", false)
}

func (productionBareBootstrapSystem) ControlPlaneBootstrapReady(ctx context.Context, cfg config.Config,
	projectID string) (bool, error) {
	return probeBareControlPlaneReadiness(ctx, cfg.HealthAddr, "/bootstrapz", projectID, true)
}

func probeBareControlPlaneReadiness(ctx context.Context, healthAddr, path, projectID string,
	validateBootstrap bool) (bool, error) {
	addr := strings.TrimSpace(healthAddr)
	if addr == "" {
		return false, errors.New("control-plane health address is empty")
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	pollCtx, cancel := context.WithTimeout(ctx, bareBootstrapPollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pollCtx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, nil
	}
	if validateBootstrap {
		var body struct {
			FormatVersion string `json:"format_version"`
			Status        string `json:"status"`
			ProjectID     string `json:"project_id"`
		}
		decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
		decoder.DisallowUnknownFields()
		// The endpoint intentionally has additional operational fields; decode a
		// generic map first so strictness applies to required identity, not growth.
		var raw map[string]any
		if err := decoder.Decode(&raw); err != nil {
			return false, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return false, errors.New("control-plane bootstrap readiness returned trailing data")
		}
		body.FormatVersion, _ = raw["format_version"].(string)
		body.Status, _ = raw["status"].(string)
		body.ProjectID, _ = raw["project_id"].(string)
		if body.FormatVersion != "flowbee.bootstrap-readiness/v1" || body.Status != "bootstrap_ready" ||
			body.ProjectID != projectID {
			return false, errors.New("control-plane bootstrap readiness identity mismatch")
		}
	}
	return true, nil
}

func (productionBareBootstrapSystem) EnsureControlPlane(ctx context.Context,
	inventory config.DriverEndpointInventory, spec bareControlPlaneSpec, actionID string) (driver.LifecycleReceipt, error) {
	var endpoint *config.DriverEndpoint
	for i := range inventory.Endpoints {
		if inventory.Endpoints[i].InstanceRef == spec.InstanceRef {
			endpoint = &inventory.Endpoints[i]
			break
		}
	}
	if endpoint == nil || endpoint.ExpectedHostID != spec.HostID || endpoint.ExpectedStoreID != spec.StoreID ||
		endpoint.ExpectedTmuxServerDomainID != spec.TmuxServerDomainID ||
		endpoint.ExpectedTmuxServerOwnership != "managed_dedicated" {
		return driver.LifecycleReceipt{}, errors.New("control-plane lifecycle target does not match exact managed Driver inventory")
	}
	token, err := readOwnerOnlySecret(endpoint.TokenFile)
	if err != nil {
		return driver.LifecycleReceipt{}, err
	}
	port := driver.NewUDSPort(endpoint.UDSPath, token)
	action := driver.NewAction(actionID, "flowbee-control-plane:"+spec.LifecycleKey, spec.TargetEpoch)
	target := driver.SessionTarget{Identity: driver.Identity{HostID: spec.HostID, StoreID: spec.StoreID,
		TmuxServerDomainID: spec.TmuxServerDomainID, TmuxServerInstanceID: spec.TmuxServerInstanceID},
		LifecycleKey: spec.LifecycleKey, TargetEpoch: spec.TargetEpoch, ProfileID: spec.ProfileID,
		WorkspaceRootID: spec.WorkspaceRootID, WorkspaceRelativePath: spec.WorkspaceRelativePath,
		LeaseID: "flowbee-control-plane-" + spec.LifecycleKey, LeaseEpoch: spec.TargetEpoch,
		PresentationName: "flowbee"}
	validatePresence := func(receipt driver.LifecycleReceipt) (driver.LifecycleReceipt, error) {
		if receipt.Status != "ensured" {
			return receipt, nil
		}
		presence, presenceErr := port.LifecycleTargetPresence(ctx, spec.LifecycleKey, spec.TargetEpoch)
		if presenceErr != nil {
			return driver.LifecycleReceipt{}, presenceErr
		}
		want, got := receipt.IdentityAfter, presence.Identity
		if presence.Presence != "present" || presence.ObservedAt == "" ||
			got.HostID != want.HostID || got.StoreID != want.StoreID ||
			got.TmuxServerDomainID != want.TmuxServerDomainID ||
			got.TmuxServerInstanceID != want.TmuxServerInstanceID ||
			got.LifecycleKey != spec.LifecycleKey || got.TargetEpoch != spec.TargetEpoch ||
			got.SessionID != want.SessionID || got.PaneInstanceID != want.PaneInstanceID ||
			got.AgentRunID != want.AgentRunID {
			return driver.LifecycleReceipt{}, errors.New("control-plane listener has no exact current Driver lifecycle presence")
		}
		return receipt, nil
	}
	if existing, found, lookupErr := port.LifecycleReceiptByAction(ctx, actionID, spec.LifecycleKey, spec.TargetEpoch); lookupErr != nil {
		return driver.LifecycleReceipt{}, lookupErr
	} else if found {
		return validatePresence(existing)
	}
	receipt, ensureErr := port.EnsureLifecycleSession(ctx, target, action)
	if ensureErr == nil || errors.Is(ensureErr, driver.ErrUncertain) {
		return validatePresence(receipt)
	}
	// A response may be lost after Driver durably accepted the action. Reconcile
	// by the immutable action tuple before surfacing the transport error; never
	// create or resend a second lifecycle action.
	if existing, found, lookupErr := port.LifecycleReceiptByAction(ctx, actionID, spec.LifecycleKey, spec.TargetEpoch); lookupErr == nil && found {
		return validatePresence(existing)
	}
	return driver.LifecycleReceipt{}, ensureErr
}

func prepareBareBootstrap(ctx context.Context, system bareBootstrapSystem) (bareBootstrapPreflight, error) {
	preflight, err := resolveBareBootstrap(ctx, system)
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	cfg, err := system.Config()
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	inventory, _, err := system.DriverInventory()
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	if err := system.ProbeDrivers(ctx, inventory); err != nil {
		return bareBootstrapPreflight{}, fmt.Errorf("both exact Driver endpoints must be ready: %w", err)
	}
	ready, err := system.ControlPlaneReady(ctx, cfg)
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	if !ready {
		return bareBootstrapPreflight{}, errors.New("control plane is not LiveReady")
	}
	return preflight, nil
}

func resolveBareBootstrap(ctx context.Context, system bareBootstrapSystem) (bareBootstrapPreflight, error) {
	if system == nil {
		return bareBootstrapPreflight{}, errors.New("bare bootstrap system is required")
	}
	cfg, err := system.Config()
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	root, err := system.Git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(root) {
		return bareBootstrapPreflight{}, errors.New("git top-level is not a canonical absolute path")
	}
	marker, marked, err := bootstrap.ExistingProjectMarker(root)
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	var origin, projectID, repoID string
	if marked {
		origin = marker.RepositoryOrigin
		projectID, repoID, err = exactConfiguredProject(cfg, origin)
		if err != nil || projectID != marker.ProjectID {
			return bareBootstrapPreflight{}, errors.New("private project marker is no longer authorized by exact project/repository config")
		}
	} else {
		originRaw, gitErr := system.Git(ctx, "-C", root, "remote", "get-url", "origin")
		if gitErr != nil {
			return bareBootstrapPreflight{}, gitErr
		}
		origin, err = bootstrap.NormalizeRepositoryOrigin(originRaw)
		if err != nil {
			return bareBootstrapPreflight{}, err
		}
		projectID, repoID, err = exactConfiguredProject(cfg, origin)
		if err != nil {
			return bareBootstrapPreflight{}, err
		}
	}
	gitInfo, err := system.Git(ctx, "-C", root, "rev-parse", "--git-path", "info")
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	if !filepath.IsAbs(gitInfo) {
		gitInfo = filepath.Join(root, gitInfo)
	}
	gitInfo, err = filepath.Abs(filepath.Clean(gitInfo))
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	_, configured, err := system.DriverInventory()
	if err != nil {
		return bareBootstrapPreflight{}, err
	}
	if !configured {
		return bareBootstrapPreflight{}, errors.New("bare bootstrap requires FLOWBEE_DRIVER_ENDPOINTS_FILE; no single Driver fallback is allowed")
	}
	return bareBootstrapPreflight{RepoRoot: root, GitInfoDir: gitInfo, Origin: origin,
		ProjectID: projectID, RepositoryID: repoID}, nil
}

type fixedBareOrigin string

// resolveBareBootstrap has already normalized the discovered remote to the
// credential-free host/owner/repository identity. FileProjectInitResolver owns
// the generic first-admission validation and therefore expects an HTTPS/SSH
// spelling, not that internal canonical form. Reconstitute the one exact,
// credential-free URL here instead of weakening the resolver to accept two
// input grammars.
func (o fixedBareOrigin) ExactOrigin(context.Context, string) (string, error) {
	return "https://" + string(o) + ".git", nil
}

func executeBareBootstrap(ctx context.Context, system bareBootstrapSystem) error {
	preflight, err := resolveBareBootstrap(ctx, system)
	if err != nil {
		return err
	}
	cfg, err := system.Config()
	if err != nil {
		return err
	}
	inventory, configured, err := system.DriverInventory()
	if err != nil || !configured {
		return errors.New("bare bootstrap exact Driver inventory is unavailable")
	}
	if _, err := resolveBareBootstrapTopology(cfg, inventory, preflight.ProjectID); err != nil {
		return err
	}
	init, err := (bootstrap.FileProjectInitResolver{RepoRoot: preflight.RepoRoot,
		GitInfoDir: preflight.GitInfoDir, RequestedProjectID: preflight.ProjectID,
		Origins: fixedBareOrigin(preflight.Origin)}).ResolveProjectInit(ctx, preflight.ProjectID)
	if err != nil {
		return err
	}
	plan, err := buildBareServerActionPlan(cfg, inventory, preflight, init)
	if err != nil {
		return err
	}
	ledgerPath, err := defaultBootstrapLedgerPath(preflight.ProjectID)
	if err != nil {
		return err
	}
	db, checkpointStore, err := bootstrap.OpenSQLiteCheckpointStore(ctx, ledgerPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := initializeBarePlanCheckpoint(ctx, checkpointStore, plan); err != nil {
		return err
	}
	if err := ensureBareDriverServices(ctx, system, inventory, plan, checkpointStore); err != nil {
		return err
	}
	if err := ensureBareControlPlane(ctx, system, cfg, inventory, plan, checkpointStore); err != nil {
		return err
	}
	ready, err := system.ControlPlaneBootstrapReady(ctx, cfg, plan.ProjectID)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("control plane is not bootstrap-ready; immutable bootstrap plan is saved for resume and no unpinned launch fallback is allowed")
	}
	baseURL, err := bareBootstrapPrivateURL(cfg.PrivateAddr)
	if err != nil {
		return err
	}
	client, err := bootstrapAPIClientFromConfiguredBearer(baseURL, &http.Client{Timeout: bareBootstrapPollTimeout})
	if err != nil {
		return err
	}
	attach := func(intent bootstrap.AttachIntentSpec) error {
		inside := strings.TrimSpace(os.Getenv("TMUX")) != ""
		domain := ""
		if inside {
			domain = strings.TrimSpace(os.Getenv("FLOWBEE_CURRENT_TMUX_SERVER_DOMAIN"))
		}
		return attachHumanToInteractor(intent, inside, domain, osTmuxAttachRunner{})
	}
	return (bareServerActionRunner{Store: checkpointStore, Client: client,
		PollInterval: 500 * time.Millisecond, Attach: attach,
		FinalReady: func(callCtx context.Context) (bool, error) {
			return system.ControlPlaneReady(callCtx, cfg)
		}}).Run(ctx, plan)
}

func ensureBareControlPlane(ctx context.Context, system bareBootstrapSystem, cfg config.Config,
	inventory config.DriverEndpointInventory, plan bareServerActionPlan,
	checkpointStore bootstrap.CheckpointStore) error {
	runner := bareServerActionRunner{Store: checkpointStore}
	cp, err := initializeBarePlanCheckpoint(ctx, checkpointStore, plan)
	if err != nil {
		return err
	}
	key := "control_plane:" + plan.ProjectID
	actionID := deterministicBareActionID(plan.BootstrapID, key)
	for {
		ready, readyErr := system.ControlPlaneReady(ctx, cfg)
		if readyErr != nil {
			return readyErr
		}
		if ready {
			if cp.Completed[key] == "" {
				_, err = runner.advance(ctx, cp, func(next *bootstrap.Checkpoint) {
					next.Completed[key], next.LastHold = "healthz:live", ""
				})
			}
			return err
		}
		if cp.Prepared[key] == "" {
			cp, err = runner.advance(ctx, cp, func(next *bootstrap.Checkpoint) { next.Prepared[key] = actionID })
			if err != nil {
				return err
			}
			continue
		}
		if cp.Prepared[key] != actionID {
			return errors.New("control-plane lifecycle prepared action identity changed")
		}
		receipt, ensureErr := system.EnsureControlPlane(ctx, inventory, plan.ControlPlane, actionID)
		if ensureErr != nil {
			return ensureErr
		}
		if receipt.LifecycleReceiptID == "" || receipt.ActionID != actionID ||
			receipt.LifecycleKey != plan.ControlPlane.LifecycleKey || receipt.TargetEpoch != plan.ControlPlane.TargetEpoch {
			return errors.New("control-plane lifecycle returned mismatched durable receipt")
		}
		if prior := cp.Issued[key]; prior != "" && prior != receipt.LifecycleReceiptID {
			return errors.New("control-plane lifecycle replay returned a different durable receipt")
		}
		if cp.Issued[key] == "" {
			cp, err = runner.advance(ctx, cp, func(next *bootstrap.Checkpoint) {
				next.Issued[key] = receipt.LifecycleReceiptID
			})
			if err != nil {
				return err
			}
		}
		switch {
		case receipt.Status == "ensured":
			if receipt.IdentityAfter.HostID != plan.ControlPlane.HostID ||
				receipt.IdentityAfter.StoreID != plan.ControlPlane.StoreID ||
				receipt.IdentityAfter.TmuxServerDomainID != plan.ControlPlane.TmuxServerDomainID ||
				receipt.IdentityAfter.TmuxServerInstanceID != plan.ControlPlane.TmuxServerInstanceID ||
				receipt.IdentityAfter.SessionID == "" || receipt.IdentityAfter.PaneInstanceID == "" ||
				receipt.IdentityAfter.AgentRunID == "" || !receipt.PresentationNamePresent ||
				receipt.PresentationName != "flowbee" {
				return errors.New("control-plane lifecycle ensured the wrong incarnation or presentation")
			}
			if cp.Completed[key] == "" {
				cp, err = runner.advance(ctx, cp, func(next *bootstrap.Checkpoint) {
					next.Completed[key], next.LastHold = "driver:ensured", ""
				})
				if err != nil {
					return err
				}
			}
			bootstrapReady, bootstrapErr := system.ControlPlaneBootstrapReady(ctx, cfg, plan.ProjectID)
			if bootstrapErr != nil {
				return bootstrapErr
			}
			if bootstrapReady {
				return nil
			}
		case receipt.Uncertain():
			// Re-enter through the by-action lookup with the same immutable action.
		default:
			return fmt.Errorf("control-plane lifecycle is visibly %s (%s)", receipt.Status, receipt.DiagnosticCode)
		}
		if err := waitBootstrapPoll(ctx, 500*time.Millisecond); err != nil {
			return fmt.Errorf("control plane remains unavailable after lifecycle receipt %s: %w", receipt.LifecycleReceiptID, err)
		}
	}
}

func bareBootstrapPrivateURL(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("private API address is empty")
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	return "http://" + addr, nil
}

func ensureBareDriverServices(ctx context.Context, system bareBootstrapSystem,
	inventory config.DriverEndpointInventory, plan bareServerActionPlan,
	checkpointStore bootstrap.CheckpointStore) error {
	if err := system.ProbeDrivers(ctx, inventory); err == nil {
		return nil
	}
	runner := bareServerActionRunner{Store: checkpointStore}
	cp, err := initializeBarePlanCheckpoint(ctx, checkpointStore, plan)
	if err != nil {
		return err
	}
	for _, endpoint := range inventory.Endpoints {
		if endpoint.ServiceEnsure == nil {
			return fmt.Errorf("Driver endpoint %s is down and has no pinned service_ensure authority", endpoint.InstanceRef)
		}
		key := "driver_service:" + endpoint.InstanceRef
		actionID := deterministicBareActionID(plan.BootstrapID, key)
		if cp.Prepared[key] == "" {
			cp, err = runner.advance(ctx, cp, func(next *bootstrap.Checkpoint) { next.Prepared[key] = actionID })
			if err != nil {
				return err
			}
		}
		ensure := endpoint.ServiceEnsure
		ensurer, ensureErr := system.DriverServiceEnsurer(endpoint)
		if ensureErr != nil {
			return ensureErr
		}
		port := bootstrap.DriverServicePort{Ensurer: ensurer}
		ref := bootstrap.EndpointRef{InstanceRef: endpoint.InstanceRef, HostID: endpoint.ExpectedHostID,
			StoreID: endpoint.ExpectedStoreID, TmuxServerDomainID: endpoint.ExpectedTmuxServerDomainID,
			ServiceManagerPath: ensure.ServiceManagerPath, ServiceManagerSHA256: ensure.ServiceManagerSHA256,
			ServiceUpdateAuthorized: ensure.AllowUpdate, ReleaseID: ensure.ReleaseID,
			ExecutablePath: ensure.ExecutablePath, ExecutableSHA256: ensure.ExecutableSHA256,
			ConfigPath: ensure.ConfigPath, ConfigSHA256: ensure.ConfigSHA256, UDSPath: endpoint.UDSPath,
			RequiredContracts: ensure.RequiredContracts}
		for {
			receipt, ensureErr := port.EnsureEndpoint(ctx, ref, bootstrap.EffectRequest{ActionID: actionID,
				ProjectID: plan.ProjectID, CWD: plan.CWD})
			if ensureErr != nil {
				return ensureErr
			}
			if prior := cp.Issued[key]; prior != "" && prior != receipt.ID {
				return errors.New("Driver service replay returned a different durable receipt")
			}
			if cp.Issued[key] == "" {
				cp, err = runner.advance(ctx, cp, func(next *bootstrap.Checkpoint) { next.Issued[key] = receipt.ID })
				if err != nil {
					return err
				}
			}
			if receipt.State == "ready" {
				break
			}
			if err := waitBootstrapPoll(ctx, 500*time.Millisecond); err != nil {
				return fmt.Errorf("Driver service %s remains %s: %w", endpoint.InstanceRef, receipt.State, err)
			}
		}
	}
	for {
		if err := system.ProbeDrivers(ctx, inventory); err == nil {
			return nil
		}
		if err := waitBootstrapPoll(ctx, 500*time.Millisecond); err != nil {
			return fmt.Errorf("Driver services reported ready but exact endpoint facts remain unavailable: %w", err)
		}
	}
}

func deterministicBareActionID(bootstrapID, key string) string {
	sum := sha256.Sum256([]byte(bootstrapID + "\x00" + key))
	return "bootstrap-" + hex.EncodeToString(sum[:16])
}

func exactConfiguredProject(cfg config.Config, origin string) (string, string, error) {
	type repoCandidate struct{ repoID, origin string }
	var repositories []repoCandidate
	if len(cfg.Repos) == 0 {
		if cfg.GithubOwner != "" && cfg.GithubRepo != "" {
			canonical, err := bootstrap.NormalizeRepositoryOrigin("https://github.com/" + cfg.GithubOwner + "/" + cfg.GithubRepo + ".git")
			if err != nil {
				return "", "", err
			}
			repositories = append(repositories, repoCandidate{cfg.GithubRepo, canonical})
		}
	} else {
		for _, repo := range cfg.Repos {
			canonical, err := bootstrap.NormalizeRepositoryOrigin("https://github.com/" + repo.Owner + "/" + repo.Repo + ".git")
			if err != nil {
				return "", "", err
			}
			if repo.IsActive() {
				repositories = append(repositories, repoCandidate{repo.ID, canonical})
			}
		}
	}
	var matchedRepos []repoCandidate
	for _, repo := range repositories {
		if repo.origin == origin {
			matchedRepos = append(matchedRepos, repo)
		}
	}
	if len(matchedRepos) != 1 {
		return "", "", errors.New("repository origin must resolve to exactly one configured project/repository")
	}
	repoID := matchedRepos[0].repoID
	var matchedProjects []string
	for _, project := range cfg.BootstrapProjects {
		for _, configuredRepoID := range project.RepositoryIDs {
			if configuredRepoID == repoID {
				matchedProjects = append(matchedProjects, project.ProjectID)
				break
			}
		}
	}
	if len(matchedProjects) != 1 || matchedProjects[0] == "" {
		return "", "", errors.New("repository requires one explicit bootstrap_projects project mapping; run flowbee init/project-add")
	}
	return matchedProjects[0], repoID, nil
}

func defaultBootstrapLedgerPath(projectID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || projectID == "" || strings.ContainsAny(projectID, "/\\\x00") {
		return "", errors.New("cannot resolve private bootstrap ledger path")
	}
	sum := sha256.Sum256([]byte("flowbee-bootstrap-project/v1\x00" + projectID))
	return filepath.Join(home, ".flowbee", "bootstrap", hex.EncodeToString(sum[:16])+".db"), nil
}
