package driver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLifecycleV3FakeInjectsExactBootstrapAndReplaysSameBody(t *testing.T) {
	fake := NewFake()
	content := "ROLE reviewer\nPROJECT russ\nEPIC money-honesty\nHEAD abc BASE def\nReview only this fenced PR."
	secret := "fb_target_0123456789abcdef0123456789abcdef0123456789abcdef"
	target := SessionTarget{Identity: Identity{HostID: "host", StoreID: "store",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server"},
		LifecycleKey: "epic-worker-key", TargetEpoch: 1, ProfileID: "grok_reviewer",
		WorkspaceRootID: "flowbee", WorkspaceRelativePath: "russ/money-honesty",
		LeaseID: "lease", LeaseEpoch: 1, PresentationName: "flowbee-worker-grok-russ-money-honesty",
		Bootstrap: &LifecycleBootstrapArtifact{ArtifactID: "artifact-1", Format: "initial_prompt_utf8/v1",
			PayloadSHA256: sha256Text(content), ContentUTF8: content},
		CredentialEnvelope: &LifecycleCredentialEnvelope{EnvelopeID: "envelope-1",
			Format: "flowbee_target_bearer_utf8/v1", CredentialEpoch: 1,
			PayloadSHA256: sha256Text(secret), SecretUTF8: secret}}
	action := NewAction("ensure-v3", "public ledger payload", 1)
	action.LeaseID, action.LeaseEpoch = "lease", 1
	receipt, err := fake.EnsureLifecycleSession(context.Background(), target, action)
	if err != nil {
		t.Fatal(err)
	}
	if fake.EnsureCalls != 1 || len(fake.EnsureTargets) != 1 ||
		fake.EnsureTargets[0].Bootstrap.ContentUTF8 != content ||
		receipt.FormatVersion != "tmux-driver.lifecycle-receipt/v3" ||
		receipt.BootstrapArtifact.Status != "injected" || receipt.CredentialInstall.Status != "installed" {
		t.Fatalf("calls=%d targets=%+v receipt=%+v", fake.EnsureCalls, fake.EnsureTargets, receipt)
	}
	if _, err := fake.EnsureLifecycleSession(context.Background(), target, action); err != nil || fake.EnsureCalls != 1 {
		t.Fatalf("same-body replay calls=%d err=%v", fake.EnsureCalls, err)
	}
	changed := target
	changed.Bootstrap = &LifecycleBootstrapArtifact{ArtifactID: "artifact-1", Format: "initial_prompt_utf8/v1",
		PayloadSHA256: sha256Text(content + " changed"), ContentUTF8: content + " changed"}
	if _, err := fake.EnsureLifecycleSession(context.Background(), changed, action); !errors.Is(err, ErrIdempotencyBody) {
		t.Fatalf("changed-body replay err=%v", err)
	}
	encoded, _ := json.Marshal(receipt)
	if strings.Contains(string(encoded), secret) {
		t.Fatal("credential secret leaked into lifecycle receipt")
	}
}

func TestLifecycleV3FailsClosedWithoutEveryExactContract(t *testing.T) {
	fake := NewFake()
	fake.Meta.Contracts.LifecycleManagedDisplayName.Supported = false
	content := "exact public prompt"
	secret := "fb_target_0123456789abcdef0123456789abcdef0123456789abcdef"
	target := SessionTarget{Identity: Identity{HostID: "host", StoreID: "store",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server"}, LifecycleKey: "worker",
		TargetEpoch: 1, ProfileID: "codex_builder", WorkspaceRootID: "flowbee", WorkspaceRelativePath: "russ/e",
		LeaseID: "lease", LeaseEpoch: 1, PresentationName: "flowbee-worker-codex-russ-e",
		Bootstrap: &LifecycleBootstrapArtifact{ArtifactID: "a", Format: "initial_prompt_utf8/v1",
			PayloadSHA256: sha256Text(content), ContentUTF8: content},
		CredentialEnvelope: &LifecycleCredentialEnvelope{EnvelopeID: "c",
			Format: "flowbee_target_bearer_utf8/v1", CredentialEpoch: 1,
			PayloadSHA256: sha256Text(secret), SecretUTF8: secret}}
	if _, err := fake.EnsureLifecycleSession(context.Background(), target, NewAction("a", "p", 1)); err == nil {
		t.Fatal("v3 Ensure mutated without managed display-name contract")
	}
	if fake.EnsureCalls != 0 || len(fake.EnsureTargets) != 0 {
		t.Fatalf("fail-closed ensure calls=%d targets=%d", fake.EnsureCalls, len(fake.EnsureTargets))
	}
}

func TestLifecycleV3ProfileInventoryRejectsMissingDisabledAndProviderSwapBeforeEffect(t *testing.T) {
	content := "exact public prompt"
	secret := "fb_target_0123456789abcdef0123456789abcdef0123456789abcdef"
	target := SessionTarget{Identity: Identity{HostID: "host", StoreID: "store",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server"}, LifecycleKey: "worker",
		TargetEpoch: 1, ProfileID: "codex_builder", WorkspaceRootID: "flowbee", WorkspaceRelativePath: "russ/e",
		LeaseID: "lease", LeaseEpoch: 1, PresentationName: "flowbee-worker-codex-russ-e",
		Bootstrap: &LifecycleBootstrapArtifact{ArtifactID: "a", Format: "initial_prompt_utf8/v1",
			PayloadSHA256: sha256Text(content), ContentUTF8: content},
		CredentialEnvelope: &LifecycleCredentialEnvelope{EnvelopeID: "c",
			Format: "flowbee_target_bearer_utf8/v1", CredentialEpoch: 1,
			PayloadSHA256: sha256Text(secret), SecretUTF8: secret}}
	for _, tc := range []struct {
		name   string
		mutate func(*FakePort)
	}{
		{name: "missing", mutate: func(fake *FakePort) {
			fake.ProfileInventory.Profiles = nil
		}},
		{name: "disabled", mutate: func(fake *FakePort) {
			fake.ProfileInventory.LifecycleEnabled = false
			fake.ProfileInventory.Profiles = nil
		}},
		{name: "provider_swap", mutate: func(fake *FakePort) {
			for n := range fake.ProfileInventory.Profiles {
				if fake.ProfileInventory.Profiles[n].ProfileID == "codex_builder" {
					fake.ProfileInventory.Profiles[n].Provider = "grok"
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := NewFake()
			tc.mutate(fake)
			if _, err := fake.EnsureLifecycleSession(context.Background(), target,
				NewAction("profile-"+tc.name, "p", 1)); err == nil {
				t.Fatal("profile mismatch mutated lifecycle")
			}
			if fake.EnsureCalls != 0 || len(fake.EnsureTargets) != 0 {
				t.Fatalf("fail-closed ensure calls=%d targets=%d", fake.EnsureCalls, len(fake.EnsureTargets))
			}
		})
	}
}

func TestHTTPLifecycleProfileInventoryIsStrictAndDomainBound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown_key", mutate: func(inventory map[string]any) { inventory["future"] = true }},
		{name: "wrong_domain", mutate: func(inventory map[string]any) {
			inventory["tmux_server_domain_id"] = "other"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inventory := managedProfileInventoryFixture()
			tc.mutate(inventory)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/meta":
					_ = json.NewEncoder(w).Encode(controlOriginMetaFixture(true, true))
				case "/v2/lifecycle/profiles":
					_ = json.NewEncoder(w).Encode(inventory)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			port := &HTTPPort{BaseURL: srv.URL, Token: "secret"}
			if _, err := port.LifecycleProfiles(context.Background()); err == nil {
				t.Fatal("invalid lifecycle profile inventory accepted")
			}
		})
	}
}

func TestHTTPLifecycleProfileInventoryAcceptsAdditiveUtilityProfiles(t *testing.T) {
	inventory := managedProfileInventoryFixture()
	inventory["utility_profiles"] = []map[string]any{{
		"profile_id": "driver_stream_console", "utility_kind": "driver_stream_console",
		"ensure_supported": true, "ensure_authorized": true, "stop_supported": true, "stop_authorized": true,
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/meta":
			_ = json.NewEncoder(w).Encode(controlOriginMetaFixture(true, true))
		case "/v2/lifecycle/profiles":
			_ = json.NewEncoder(w).Encode(inventory)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := &HTTPPort{BaseURL: srv.URL, Token: "token"}
	if _, err := port.LifecycleProfiles(context.Background()); err != nil {
		t.Fatalf("additive utility profile rejected: %v", err)
	}
}

func managedProfileInventoryFixture() map[string]any {
	profile := managedProfile("codex_builder", "codex")
	raw, _ := json.Marshal(profile)
	var encoded map[string]any
	_ = json.Unmarshal(raw, &encoded)
	return map[string]any{"api_version": "v2", "server_time": "2026-07-19T12:00:00Z",
		"format_version": "tmux-driver.lifecycle-profile-inventory/v1", "lifecycle_enabled": true,
		"tmux_server_domain_id": "flowbee", "profiles": []any{encoded}}
}
