package actorprotocol_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/epicflow"
	actorprotocol "github.com/samhotchkiss/flowbee/protocol/flowbee/v2"
)

func TestActorProtocolLoadsAndCoversDeliveryRecovery(t *testing.T) {
	c, err := actorprotocol.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Version() != "2.0" {
		t.Fatalf("version=%s", c.Version())
	}
	for _, policy := range epicflow.Registry {
		r, ok := c.RecoveryForAttention(string(policy.AttentionKind))
		if !ok {
			t.Fatalf("state %s attention %s has no actor recovery contract", policy.State, policy.AttentionKind)
		}
		if r.Owner == "" || r.TestID == "" || r.Fence == "" {
			t.Fatalf("incomplete recovery for %s: %+v", policy.State, r)
		}
	}
	for _, code := range []string{"work_intent_capture_unacked", "epic_admission_outcome_uncertain", "action_delivery_uncertain", "reconciler_dead", "capacity_pool_exhausted"} {
		if _, ok := c.Recovery(code); !ok {
			t.Fatalf("missing recovery code %s", code)
		}
	}
	hash, err := actorprotocol.BundleHash()
	if err != nil || len(hash) != len("sha256:")+64 {
		t.Fatalf("bundle hash=%q err=%v", hash, err)
	}
}

func TestActorProtocolForbidsHumanMechanicalHandoff(t *testing.T) {
	c, err := actorprotocol.Load()
	if err != nil {
		t.Fatal(err)
	}
	interactor, ok := c.Role("interactor")
	if !ok {
		t.Fatal("missing interactor role")
	}
	if len(interactor.Outputs) == 0 || len(interactor.Forbidden) == 0 {
		t.Fatalf("unbounded interactor role: %+v", interactor)
	}
	if _, ok := c.Role("orchestrator"); !ok {
		t.Fatal("missing paired orchestrator role")
	}
}

func TestGeneratedActorAndRecoveryViewsAreComplete(t *testing.T) {
	c, err := actorprotocol.Load()
	if err != nil {
		t.Fatal(err)
	}
	files, err := actorprotocol.GeneratedFiles(c)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(files), len(c.Roles)+len(c.RecoveryCodes)+1; got != want {
		t.Fatalf("generated files=%d want=%d", got, want)
	}
	for path, body := range files {
		if len(body) == 0 {
			t.Fatalf("empty generated file %s", path)
		}
	}
}

func TestCheckedInActorAndRecoveryViewsMatchContract(t *testing.T) {
	c, err := actorprotocol.Load()
	if err != nil {
		t.Fatal(err)
	}
	files, err := actorprotocol.GeneratedFiles(c)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	for relative, want := range files {
		got, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatalf("read generated %s: %v", relative, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("generated %s differs; run go run ./tools/protocolgen", relative)
		}
	}
}
