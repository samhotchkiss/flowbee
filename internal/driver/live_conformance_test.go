package driver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveDriverV24Conformance is an opt-in smoke test for the installed local
// tmux-driver daemon. It deliberately goes through DriverPort only: no raw tmux,
// tmux-send, inferred pane name, CWD, or PID is ever used as authority.
//
// Read-only conformance:
//
//	FLOWBEE_DRIVER_LIVE_TEST=1 \
//	FLOWBEE_DRIVER_SOCKET=/path/to/api.sock \
//	FLOWBEE_DRIVER_TOKEN_FILE=/path/to/control.token \
//	go test ./internal/driver -run TestLiveDriverV24Conformance -v
//
// Add FLOWBEE_DRIVER_LIVE_LIFECYCLE=1 to exercise an isolated Ensure -> exact
// presence -> Stop -> positive absence cycle. The lifecycle path requires an
// existing active session only to obtain Driver's stable tmux-server incarnation
// (never its raw pane identity); overrides are available for empty daemons.
// Add FLOWBEE_DRIVER_LIVE_CONTROL_ORIGIN=1 as well to create an exact direct-
// origin grant and deliver one benign conformance marker to that isolated
// session. The control-origin path is never exercised against an existing
// operator session.
func TestLiveDriverV24Conformance(t *testing.T) {
	if os.Getenv("FLOWBEE_DRIVER_LIVE_TEST") != "1" {
		t.Skip("set FLOWBEE_DRIVER_LIVE_TEST=1 to run against an installed Driver daemon")
	}
	socket := os.Getenv("FLOWBEE_DRIVER_SOCKET")
	tokenFile := os.Getenv("FLOWBEE_DRIVER_TOKEN_FILE")
	if socket == "" || tokenFile == "" {
		t.Fatal("FLOWBEE_DRIVER_SOCKET and FLOWBEE_DRIVER_TOKEN_FILE are required")
	}
	info, statErr := os.Lstat(tokenFile)
	if statErr != nil {
		t.Fatalf("stat Driver control token: %v", statErr)
	} else if !info.Mode().IsRegular() {
		t.Fatalf("Driver control token must be a regular non-symlink file")
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("Driver control token permissions are %04o; want owner-only", info.Mode().Perm())
	}
	tokenBytes, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("read Driver control token: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		t.Fatal("Driver control token file is empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var port DriverPort = NewUDSPort(socket, token)

	meta, err := port.Metadata(ctx)
	if err != nil {
		t.Fatalf("GET /v2/meta through DriverPort: %v", err)
	}
	if meta.APIVersion != "v2" {
		t.Fatalf("Driver api_version=%q, want v2", meta.APIVersion)
	}
	if !meta.ControlPrincipalOrigin {
		t.Fatal("GET /v2/meta did not advertise features.control_principal_origin=true")
	}
	capability, err := port.ControlOriginCapability(ctx)
	if err != nil {
		t.Fatalf("authenticated GET /v2/control/capabilities through DriverPort: %v", err)
	}
	if capability.FormatVersion != controlOriginCapabilityFormat ||
		capability.PrincipalID != "flowbee-control" || !capability.Supported ||
		!capability.Authorized || len(capability.MissingScopes) != 0 {
		t.Fatalf("Driver returned non-activatable control-origin capability: %+v", capability)
	}
	snapshot, err := port.SnapshotSessions(ctx)
	if err != nil {
		t.Fatalf("session snapshot through DriverPort: %v", err)
	}
	if snapshot.HostID != meta.HostID || snapshot.StoreID != meta.StoreID {
		t.Fatalf("snapshot cursor domain %s/%s differs from metadata %s/%s",
			snapshot.HostID, snapshot.StoreID, meta.HostID, meta.StoreID)
	}
	// An empty cursor is Driver's documented initial-replay request. The
	// replay_floor_cursor is a lower bound, not itself guaranteed to be a valid
	// exclusive `after` cursor.
	batch, err := port.Observe(ctx, "")
	if err != nil {
		t.Fatalf("observation replay through DriverPort: %v", err)
	}
	if batch.CursorGap || batch.StoreReset {
		t.Fatalf("fresh replay unexpectedly required recovery: gap=%v reset=%v", batch.CursorGap, batch.StoreReset)
	}
	for _, event := range batch.Events {
		if event.Identity.StoreID != meta.StoreID {
			t.Fatalf("observation %s crossed store domain: got %s want %s",
				event.EventID, event.Identity.StoreID, meta.StoreID)
		}
	}
	t.Logf("v2.4 read/control capability green: driver=%s host=%s store=%s sessions=%d events=%d",
		meta.Instance, meta.HostID, meta.StoreID, len(snapshot.Sessions), len(batch.Events))

	if os.Getenv("FLOWBEE_DRIVER_LIVE_LIFECYCLE") != "1" {
		if os.Getenv("FLOWBEE_DRIVER_LIVE_CONTROL_ORIGIN") == "1" {
			t.Fatal("FLOWBEE_DRIVER_LIVE_CONTROL_ORIGIN=1 requires FLOWBEE_DRIVER_LIVE_LIFECYCLE=1")
		}
		return
	}
	runLiveLifecycleConformance(t, ctx, port, meta, snapshot, capability)
}

func runLiveLifecycleConformance(t *testing.T, ctx context.Context, port DriverPort,
	meta DriverMetadata, snapshot SessionSnapshot, capability ControlOriginCapability,
) {
	t.Helper()
	serverID := os.Getenv("FLOWBEE_DRIVER_TMUX_SERVER_INSTANCE_ID")
	if serverID == "" {
		for _, session := range snapshot.Sessions {
			if session.Lifecycle != "ended" && session.Identity.TmuxServerInstanceID != "" {
				serverID = session.Identity.TmuxServerInstanceID
				break
			}
		}
	}
	// An ended session may still provide the stable server incarnation recorded
	// by Driver. The lifecycle Ensure call rechecks it against the current server
	// and fails closed if the server has restarted since that observation.
	if serverID == "" {
		for _, session := range snapshot.Sessions {
			if session.Identity.TmuxServerInstanceID != "" {
				serverID = session.Identity.TmuxServerInstanceID
				break
			}
		}
	}
	if serverID == "" {
		t.Fatal("lifecycle conformance needs FLOWBEE_DRIVER_TMUX_SERVER_INSTANCE_ID when the snapshot has no active session")
	}
	profile := envDefault("FLOWBEE_DRIVER_LIVE_PROFILE_ID", "codex_builder")
	workspaceRoot := envDefault("FLOWBEE_DRIVER_LIVE_WORKSPACE_ROOT_ID", "flowbee")
	workspacePath := envDefault("FLOWBEE_DRIVER_LIVE_WORKSPACE_RELATIVE_PATH", "flowbee")
	key := "flowbee-conformance-" + randomHex(t, 8)
	leaseID := randomUUID(t)
	target := SessionTarget{
		Identity: Identity{HostID: meta.HostID, StoreID: meta.StoreID,
			TmuxServerInstanceID: serverID},
		LifecycleKey: key, TargetEpoch: 1, ProfileID: profile,
		WorkspaceRootID: workspaceRoot, WorkspaceRelativePath: workspacePath,
		LeaseID: leaseID, LeaseEpoch: 1,
	}
	ensureAction := NewAction(randomUUID(t), "live lifecycle conformance ensure", 1)
	ensured, err := port.EnsureLifecycleSession(ctx, target, ensureAction)
	if err != nil {
		t.Fatalf("lifecycle ensure through DriverPort: %v", err)
	}
	target.Identity = ensured.IdentityAfter
	stopAction := NewAction(randomUUID(t), "live lifecycle conformance stop", 1)
	stopped := false
	defer func() {
		if stopped {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = stopAndResolve(cleanupCtx, port, target, stopAction)
	}()

	presence, err := port.LifecycleTargetPresence(ctx, key, 1)
	if err != nil {
		t.Fatalf("lifecycle presence after ensure: %v", err)
	}
	if presence.Presence != "present" || presence.Identity.SessionID != ensured.IdentityAfter.SessionID {
		t.Fatalf("lifecycle presence after ensure=%+v; receipt identity=%+v", presence, ensured.IdentityAfter)
	}
	if os.Getenv("FLOWBEE_DRIVER_LIVE_CONTROL_ORIGIN") == "1" {
		runLiveControlOriginConformance(t, ctx, port, target, capability)
	}
	receipt, err := stopAndResolve(ctx, port, target, stopAction)
	if err != nil {
		t.Fatalf("exact lifecycle stop through DriverPort: %v", err)
	}
	if receipt.Status != "stopped" && receipt.Status != "target_absent" {
		t.Fatalf("lifecycle stop status=%q", receipt.Status)
	}
	absent, err := port.LifecycleTargetPresence(ctx, key, 1)
	if err != nil {
		t.Fatalf("lifecycle presence after stop: %v", err)
	}
	if !absent.ExactAbsent() {
		t.Fatalf("lifecycle target lacks positive absence after stop: %+v", absent)
	}
	stopped = true
	t.Logf("lifecycle conformance green: profile=%s key=%s receipt=%s", profile, key, receipt.LifecycleReceiptID)
}

// runLiveControlOriginConformance proves the v2.4 Flowbee-authored delivery
// contract against a session created solely for this test. The first response is
// intentionally treated as lost local state: recovery begins from Driver's
// durable by-action index, then an exact request replay must resolve to the same
// delivery rather than creating a second transport effect.
func runLiveControlOriginConformance(t *testing.T, ctx context.Context, port DriverPort,
	target SessionTarget, capability ControlOriginCapability,
) {
	t.Helper()
	if capability.PrincipalID != "flowbee-control" || !capability.Authorized {
		t.Fatalf("control-origin mutation attempted without exact authenticated capability: %+v", capability)
	}
	if target.Identity.SessionID == "" || target.Identity.PaneInstanceID == "" {
		t.Fatalf("isolated recipient lacks stable incarnation identity: %+v", target.Identity)
	}

	grant := Grant{
		GrantID: randomUUID(t), SenderPrincipalID: capability.PrincipalID,
		RecipientSessionID:      target.Identity.SessionID,
		RecipientPaneInstanceID: target.Identity.PaneInstanceID,
		Epoch:                   1, MaximumPayloadBytes: 4096,
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano),
	}
	if err := port.Grant(ctx, grant); err != nil {
		t.Fatalf("create exact v2.4 control-origin grant: %v", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := port.RevokeGrant(cleanupCtx, grant.GrantID, grant.Epoch); err != nil {
			t.Logf("cleanup control-origin grant %s: %v", grant.GrantID, err)
		}
	}()

	payload := "Flowbee tmux-driver v2.4 transport conformance marker " + randomHex(t, 8) +
		". This is a transport-only test; no product action is requested."
	action := NewAction(randomUUID(t), payload, grant.Epoch)
	action.SenderPrincipalID = capability.PrincipalID
	req := SendRequest{
		Action: action, GrantID: grant.GrantID,
		RecipientSessionID:      target.Identity.SessionID,
		RecipientPaneInstanceID: target.Identity.PaneInstanceID,
		GrantEpoch:              grant.Epoch,
		// v2.4 direct-origin sends must never impersonate a managed session.
		OnBehalfOfSessionID: "",
	}
	first, err := port.Send(ctx, req)
	if err != nil {
		t.Fatalf("send v2.4 direct-origin message: %v", err)
	}
	if first.SenderPrincipalID != capability.PrincipalID || first.Sender.SessionID != "" ||
		first.ActionID != action.ActionID || first.GrantID != grant.GrantID ||
		first.Recipient.SessionID != target.Identity.SessionID ||
		first.Recipient.PaneInstanceID != target.Identity.PaneInstanceID ||
		first.PayloadSHA256 != action.PayloadSHA256 {
		t.Fatalf("control-origin receipt changed immutable delivery identity: %+v", first)
	}
	if !first.Submitted() {
		t.Fatalf("control-origin receipt lacks terminal insertion evidence: %+v", first)
	}
	if first.StageComplete() {
		t.Fatal("Driver transport receipt was incorrectly treated as Flowbee stage success")
	}

	// Simulate Flowbee crashing after Driver completed the effect but before the
	// local SQL receipt transaction. Recovery is by authenticated control-owner
	// lookup; no sender_session_id selector is supplied.
	action.GrantID, action.GrantEpoch = grant.GrantID, grant.Epoch
	action.RecipientSessionID, action.RecipientPaneInstanceID = target.Identity.SessionID, target.Identity.PaneInstanceID
	recovered, found, err := port.ReceiptByAction(ctx, action.ExpectedReceipt())
	if err != nil {
		t.Fatalf("recover lost control-origin receipt by action: %v", err)
	}
	if !found || recovered.DeliveryID != first.DeliveryID ||
		recovered.SenderPrincipalID != capability.PrincipalID || recovered.StageComplete() {
		t.Fatalf("by-action recovery did not return exact transport-only receipt: found=%v receipt=%+v", found, recovered)
	}

	// An exact idempotency replay is observable as the same durable delivery ID.
	// Driver's v2.4 contract guarantees this path performs no second terminal
	// mutation; a different ID would mechanically prove a violation.
	replayed, err := port.Send(ctx, req)
	if err != nil {
		t.Fatalf("exact control-origin replay: %v", err)
	}
	if replayed.DeliveryID != first.DeliveryID || replayed.ActionID != first.ActionID ||
		replayed.PayloadSHA256 != first.PayloadSHA256 || replayed.Status != first.Status {
		t.Fatalf("exact replay created or changed a delivery: first=%+v replay=%+v", first, replayed)
	}
	if replayed.StageComplete() {
		t.Fatal("replayed transport receipt was incorrectly treated as Flowbee stage success")
	}
	t.Logf("v2.4 direct-origin delivery green: grant=%s delivery=%s action=%s",
		grant.GrantID, first.DeliveryID, first.ActionID)
}

func stopAndResolve(ctx context.Context, port DriverPort, target SessionTarget, action Action) (LifecycleReceipt, error) {
	receipt, err := port.StopSession(ctx, target, action)
	if !errors.Is(err, ErrUncertain) {
		return receipt, err
	}
	action.Epoch++
	return port.VerifyLifecycleEffect(ctx, receipt.LifecycleReceiptID, target, action)
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func randomHex(t *testing.T, bytes int) string {
	t.Helper()
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func randomUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
