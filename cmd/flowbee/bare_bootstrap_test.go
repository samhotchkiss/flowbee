package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/bootstrap"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/driver"
)

type bareSystemFake struct {
	cfg                     config.Config
	inventory               config.DriverEndpointInventory
	driversReady, liveReady bool
	gitCalls                [][]string
	driverProbes            int
	controlProbes           int
	bootstrapReady          bool
	controlEnsureCalls      []string
	controlReceipts         []driver.LifecycleReceipt
	controlEnsureErr        error
	serviceEnsurer          bootstrap.DriverServiceEnsurer
	driverReadyAfter        int
}

func TestBareBootstrapOverallDeadlineAllowsSequentialServiceEnsures(t *testing.T) {
	t.Setenv(bareBootstrapTimeoutEnv, "")
	got, err := bareBootstrapOverallTimeout()
	if err != nil || got != 5*time.Minute || got <= 2*30*time.Second {
		t.Fatalf("default timeout=%s err=%v", got, err)
	}
	t.Setenv(bareBootstrapTimeoutEnv, "8m")
	if got, err = bareBootstrapOverallTimeout(); err != nil || got != 8*time.Minute {
		t.Fatalf("configured timeout=%s err=%v", got, err)
	}
	for _, invalid := range []string{"garbage", "29s", "31m"} {
		t.Setenv(bareBootstrapTimeoutEnv, invalid)
		if _, err := bareBootstrapOverallTimeout(); err == nil {
			t.Fatalf("invalid timeout %q accepted", invalid)
		}
	}
}

func (f *bareSystemFake) Config() (config.Config, error) { return f.cfg, nil }
func (f *bareSystemFake) Git(_ context.Context, args ...string) (string, error) {
	f.gitCalls = append(f.gitCalls, append([]string(nil), args...))
	switch {
	case reflect.DeepEqual(args, []string{"rev-parse", "--show-toplevel"}):
		return f.cfg.DatabaseURL, nil // test stores its canonical repo root here
	case len(args) == 5 && args[2] == "remote":
		return "git@github.com:Sam/Russ.git", nil
	case len(args) == 5 && args[2] == "rev-parse":
		return filepath.Join(f.cfg.DatabaseURL, ".git", "info"), nil
	default:
		return "", errors.New("unexpected git invocation")
	}
}
func (f *bareSystemFake) DriverInventory() (config.DriverEndpointInventory, bool, error) {
	return f.inventory, true, nil
}
func (f *bareSystemFake) DriverServiceEnsurer(config.DriverEndpoint) (bootstrap.DriverServiceEnsurer, error) {
	if f.serviceEnsurer == nil {
		return nil, errors.New("unexpected Driver service Ensure")
	}
	return f.serviceEnsurer, nil
}
func (f *bareSystemFake) ProbeDrivers(context.Context, config.DriverEndpointInventory) error {
	f.driverProbes++
	if !f.driversReady && (f.driverReadyAfter == 0 || f.driverProbes < f.driverReadyAfter) {
		return errors.New("managed endpoint down")
	}
	return nil
}

type serviceSequenceEnsurer struct {
	statuses []string
	actions  []string
}

func (f *serviceSequenceEnsurer) EnsureDriverService(_ context.Context,
	req bootstrap.DriverServiceEnsureRequest) (bootstrap.DriverServiceEnsureReceipt, error) {
	f.actions = append(f.actions, req.ActionID)
	status := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	receipt := bootstrap.DriverServiceEnsureReceipt{FormatVersion: bootstrap.DriverServiceEnsureReceiptFormat,
		ServiceReceiptID: "service-receipt", ActionID: req.ActionID,
		RequestFingerprint: "sha256:" + strings.Repeat("d", 64), Status: status,
		Change: "none", ReleaseID: req.ReleaseID, ExecutablePath: req.ExecutablePath,
		ExecutableSHA256: req.ExecutableSHA256, ConfigPath: req.ConfigPath, ConfigSHA256: req.ConfigSHA256,
		Label: "local.tmux-driver.test", Destination: "/tmp/tmux-driver.plist", UDSPath: "/tmp/driver.sock",
		Contracts: req.RequiredContracts, AcceptedAt: "2026-07-19T12:00:00Z"}
	if status == "ready" {
		receipt.Readiness, receipt.CompletedAt, receipt.PID = "ready", "2026-07-19T12:00:01Z", 123
		receipt.StoreID, receipt.ServerDomainID = req.ExpectedStoreID, req.ExpectedDomainID
	} else {
		receipt.Readiness = "pending"
	}
	return receipt, nil
}
func (f *bareSystemFake) ControlPlaneReady(context.Context, config.Config) (bool, error) {
	f.controlProbes++
	return f.liveReady, nil
}
func (f *bareSystemFake) ControlPlaneBootstrapReady(context.Context, config.Config, string) (bool, error) {
	return f.liveReady || f.bootstrapReady, nil
}
func (f *bareSystemFake) EnsureControlPlane(_ context.Context, _ config.DriverEndpointInventory,
	_ bareControlPlaneSpec, actionID string) (driver.LifecycleReceipt, error) {
	f.controlEnsureCalls = append(f.controlEnsureCalls, actionID)
	if f.controlEnsureErr != nil {
		return driver.LifecycleReceipt{}, f.controlEnsureErr
	}
	if len(f.controlReceipts) == 0 {
		return driver.LifecycleReceipt{}, errors.New("unexpected control-plane Ensure")
	}
	receipt := f.controlReceipts[0]
	if len(f.controlReceipts) > 1 {
		f.controlReceipts = f.controlReceipts[1:]
	}
	if receipt.Status == "ensured" {
		f.bootstrapReady = true
	}
	return receipt, nil
}

func TestBareBootstrapPreflightIsIdempotentAndReadOnly(t *testing.T) {
	root := t.TempDir()
	fake := &bareSystemFake{cfg: config.Config{DatabaseURL: root, GithubOwner: "sam", GithubRepo: "russ",
		BootstrapProjects: []config.BootstrapProjectConfig{{ProjectID: "russ", RepositoryIDs: []string{"russ"}}}},
		driversReady: true, liveReady: true}
	first, err := prepareBareBootstrap(context.Background(), fake)
	if err != nil {
		t.Fatalf("first preflight error = %v", err)
	}
	second, err := prepareBareBootstrap(context.Background(), fake)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent preflight = %+v / %+v, %v", first, second, err)
	}
	if first.ProjectID != "russ" || first.RepositoryID != "russ" ||
		first.Origin != "github.com/sam/russ" {
		t.Fatalf("exact resolution = %+v", first)
	}
	// The preflight interface exposes reads only. Reaching this explicit gap
	// therefore cannot half-create a project, actor, seat, or attach intent.
	if fake.driverProbes != 2 || fake.controlProbes != 2 {
		t.Fatalf("read-only probes driver=%d control=%d", fake.driverProbes, fake.controlProbes)
	}
}

func TestBareBootstrapRequiresBothDriversBeforeControlPlaneOrProductState(t *testing.T) {
	root := t.TempDir()
	fake := &bareSystemFake{cfg: config.Config{DatabaseURL: root, GithubOwner: "sam", GithubRepo: "russ",
		BootstrapProjects: []config.BootstrapProjectConfig{{ProjectID: "russ", RepositoryIDs: []string{"russ"}}}}}
	if _, err := prepareBareBootstrap(context.Background(), fake); err == nil {
		t.Fatal("missing managed endpoint was accepted")
	}
	if fake.driverProbes != 1 || fake.controlProbes != 0 {
		t.Fatalf("order driver=%d control=%d", fake.driverProbes, fake.controlProbes)
	}
}

func TestBootstrapLedgerIsSeparateFromLiveControlPlaneDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := defaultBootstrapLedgerPath("russ")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(home, ".flowbee", "bootstrap") || filepath.Base(path) == "russ.db" ||
		path == filepath.Join(home, ".flowbee", "flowbee.db") {
		t.Fatalf("ledger path = %q", path)
	}
	if same, _ := defaultBootstrapLedgerPath("russ"); same != path {
		t.Fatalf("ledger path is not deterministic: %q / %q", path, same)
	}
	if _, err := defaultBootstrapLedgerPath("../russ"); err == nil {
		t.Fatal("path-shaped project id was accepted")
	}
}

func TestExactConfiguredProjectRejectsAmbiguousOrMissingOrigin(t *testing.T) {
	cfg := config.Config{Repos: []config.RepoConfig{
		{ID: "one", Owner: "sam", Repo: "russ"},
		{ID: "two", Owner: "SAM", Repo: "RUSS"},
	}, BootstrapProjects: []config.BootstrapProjectConfig{{ProjectID: "project", RepositoryIDs: []string{"one", "two"}}}}
	if _, _, err := exactConfiguredProject(cfg, "github.com/sam/russ"); err == nil {
		t.Fatal("ambiguous configured origin was accepted")
	}
	if _, _, err := exactConfiguredProject(config.Config{}, "github.com/sam/russ"); err == nil {
		t.Fatal("unconfigured origin was accepted")
	}
}

func TestBareControlPlaneAcceptedReceiptRecoversSameActionUntilBootstrapReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	db, checkpoints, err := bootstrap.OpenSQLiteCheckpointStore(ctx, filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spec := bareControlPlaneSpec{InstanceRef: "managed", HostID: "host", StoreID: "store",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server", LifecycleKey: "flowbee-control",
		TargetEpoch: 1, ProfileID: "flowbee_control", WorkspaceRootID: "dev", WorkspaceRelativePath: "flowbee"}
	plan := bareServerActionPlan{BootstrapID: "bootstrap-russ", ProjectID: "russ", CWD: "/dev/russ",
		RepositoryOrigin: "github.com/sam/russ", ControlPlane: spec}
	actionID := deterministicBareActionID(plan.BootstrapID, "control_plane:russ")
	base := driver.LifecycleReceipt{FormatVersion: "tmux-driver.lifecycle-receipt/v3",
		LifecycleReceiptID: "lifecycle-receipt", ActionID: actionID, ActionEpoch: 1,
		LifecycleKey: spec.LifecycleKey, TargetEpoch: 1, TmuxServerDomainID: "flowbee"}
	accepted := base
	accepted.Status = "accepted"
	ensured := base
	ensured.Status = "ensured"
	ensured.IdentityAfter = driver.Identity{HostID: "host", StoreID: "store", TmuxServerDomainID: "flowbee",
		TmuxServerInstanceID: "server", SessionID: "session", PaneInstanceID: "pane", AgentRunID: "run"}
	ensured.PresentationNamePresent, ensured.PresentationName = true, "flowbee"
	fake := &bareSystemFake{controlReceipts: []driver.LifecycleReceipt{accepted, ensured}}
	if err := ensureBareControlPlane(ctx, fake, config.Config{HealthAddr: "unused"},
		config.DriverEndpointInventory{}, plan, checkpoints); err != nil {
		t.Fatal(err)
	}
	if len(fake.controlEnsureCalls) != 2 || fake.controlEnsureCalls[0] != actionID ||
		fake.controlEnsureCalls[1] != actionID {
		t.Fatalf("control Ensure action calls=%v", fake.controlEnsureCalls)
	}
	cp, ok, err := checkpoints.Load(ctx, plan.BootstrapID)
	if err != nil || !ok || cp.Prepared["control_plane:russ"] != actionID ||
		cp.Issued["control_plane:russ"] != "lifecycle-receipt" ||
		cp.Completed["control_plane:russ"] != "driver:ensured" {
		t.Fatalf("checkpoint=%+v ok=%v err=%v", cp, ok, err)
	}
}

func TestBareDriverServiceAcceptedReceiptPollsSameActionToReadyInOneInvocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	db, checkpoints, err := bootstrap.OpenSQLiteCheckpointStore(ctx, filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ensurer := &serviceSequenceEnsurer{statuses: []string{"accepted", "ready"}}
	fake := &bareSystemFake{serviceEnsurer: ensurer, driverReadyAfter: 2}
	endpoint := config.DriverEndpoint{InstanceRef: "managed", UDSPath: "/tmp/driver.sock",
		ExpectedHostID: "host", ExpectedStoreID: "store", ExpectedTmuxServerDomainID: "flowbee",
		ServiceEnsure: &config.DriverServiceEnsureConfig{ReleaseID: "release", ExecutablePath: "/opt/driver",
			ExecutableSHA256: "sha256:" + strings.Repeat("a", 64), ConfigPath: "/etc/driver.toml",
			ConfigSHA256: "sha256:" + strings.Repeat("b", 64), RequiredContracts: map[string]string{"api": "v2.5"}}}
	plan := bareServerActionPlan{BootstrapID: "bootstrap-russ", ProjectID: "russ", CWD: "/dev/russ",
		RepositoryOrigin: "github.com/sam/russ"}
	if err := ensureBareDriverServices(ctx, fake, config.DriverEndpointInventory{Endpoints: []config.DriverEndpoint{endpoint}},
		plan, checkpoints); err != nil {
		t.Fatal(err)
	}
	wantAction := deterministicBareActionID(plan.BootstrapID, "driver_service:managed")
	if len(ensurer.actions) != 2 || ensurer.actions[0] != wantAction || ensurer.actions[1] != wantAction {
		t.Fatalf("service actions=%v", ensurer.actions)
	}
	cp, ok, err := checkpoints.Load(ctx, plan.BootstrapID)
	if err != nil || !ok || cp.Prepared["driver_service:managed"] != wantAction ||
		cp.Issued["driver_service:managed"] != "service-receipt" {
		t.Fatalf("checkpoint=%+v ok=%v err=%v", cp, ok, err)
	}
}

func TestBareControlPlaneServerDeathResumesPreparedActionWithoutReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	db, checkpoints, err := bootstrap.OpenSQLiteCheckpointStore(ctx, filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spec := bareControlPlaneSpec{InstanceRef: "managed", HostID: "host", StoreID: "store",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server", LifecycleKey: "flowbee-control",
		TargetEpoch: 4, ProfileID: "flowbee_control", WorkspaceRootID: "dev", WorkspaceRelativePath: "flowbee"}
	plan := bareServerActionPlan{BootstrapID: "bootstrap-russ", ProjectID: "russ", CWD: "/dev/russ",
		RepositoryOrigin: "github.com/sam/russ", ControlPlane: spec}
	first := &bareSystemFake{controlEnsureErr: errors.New("serve died after launch request")}
	if err := ensureBareControlPlane(ctx, first, config.Config{}, config.DriverEndpointInventory{}, plan, checkpoints); err == nil {
		t.Fatal("lost control-plane launch response was reported as success")
	}
	wantAction := deterministicBareActionID(plan.BootstrapID, "control_plane:russ")
	cp, ok, err := checkpoints.Load(ctx, plan.BootstrapID)
	if err != nil || !ok || cp.Prepared["control_plane:russ"] != wantAction || cp.Issued["control_plane:russ"] != "" {
		t.Fatalf("post-death checkpoint=%+v ok=%v err=%v", cp, ok, err)
	}
	receipt := driver.LifecycleReceipt{FormatVersion: "tmux-driver.lifecycle-receipt/v3",
		LifecycleReceiptID: "recovered-receipt", ActionID: wantAction, ActionEpoch: 4,
		LifecycleKey: spec.LifecycleKey, TargetEpoch: 4, TmuxServerDomainID: "flowbee", Status: "ensured",
		IdentityAfter: driver.Identity{HostID: "host", StoreID: "store", TmuxServerDomainID: "flowbee",
			TmuxServerInstanceID: "server", SessionID: "session", PaneInstanceID: "pane", AgentRunID: "run"},
		PresentationNamePresent: true, PresentationName: "flowbee"}
	second := &bareSystemFake{controlReceipts: []driver.LifecycleReceipt{receipt}}
	if err := ensureBareControlPlane(ctx, second, config.Config{}, config.DriverEndpointInventory{}, plan, checkpoints); err != nil {
		t.Fatal(err)
	}
	if len(first.controlEnsureCalls) != 1 || first.controlEnsureCalls[0] != wantAction ||
		len(second.controlEnsureCalls) != 1 || second.controlEnsureCalls[0] != wantAction {
		t.Fatalf("death/resume actions first=%v second=%v", first.controlEnsureCalls, second.controlEnsureCalls)
	}
}

func TestBareControlPlaneRejectsStaleBootstrapListenerWhenDriverTargetIsAbsent(t *testing.T) {
	ctx := context.Background()
	db, checkpoints, err := bootstrap.OpenSQLiteCheckpointStore(ctx, filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spec := bareControlPlaneSpec{InstanceRef: "managed", HostID: "host", StoreID: "store",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server", LifecycleKey: "flowbee-control",
		TargetEpoch: 1, ProfileID: "flowbee_control", WorkspaceRootID: "dev", WorkspaceRelativePath: "flowbee"}
	plan := bareServerActionPlan{BootstrapID: "bootstrap-russ", ProjectID: "russ", CWD: "/dev/russ",
		RepositoryOrigin: "github.com/sam/russ", ControlPlane: spec}
	cp, err := initializeBarePlanCheckpoint(ctx, checkpoints, plan)
	if err != nil {
		t.Fatal(err)
	}
	key := "control_plane:russ"
	actionID := deterministicBareActionID(plan.BootstrapID, key)
	cp.Prepared[key], cp.Issued[key], cp.Completed[key] = actionID, "old-receipt", "driver:ensured"
	if _, err := checkpoints.CompareAndSwap(ctx, cp, cp.Version); err != nil {
		t.Fatal(err)
	}
	// /bootstrapz is reachable for the same project, but the exact lifecycle
	// presence probe reports the managed target absent. Readiness must not accept
	// the stale listener merely because the checkpoint once saw an ensured receipt.
	fake := &bareSystemFake{bootstrapReady: true,
		controlEnsureErr: errors.New("control-plane listener has no exact current Driver lifecycle presence")}
	if err := ensureBareControlPlane(ctx, fake, config.Config{}, config.DriverEndpointInventory{}, plan, checkpoints); err == nil ||
		len(fake.controlEnsureCalls) != 1 || fake.controlEnsureCalls[0] != actionID {
		t.Fatalf("stale listener err=%v ensure_calls=%v", err, fake.controlEnsureCalls)
	}
}
