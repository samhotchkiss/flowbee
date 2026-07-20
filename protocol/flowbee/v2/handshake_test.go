package actorprotocol_test

import (
	"strings"
	"testing"

	actorprotocol "github.com/samhotchkiss/flowbee/protocol/flowbee/v2"
)

func TestActorHandshakeIsFailClosedAndSchemaExact(t *testing.T) {
	c, err := actorprotocol.Load()
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := actorprotocol.BundleHash()
	schemas := map[string]string{}
	for _, schema := range c.RequiredSchemas {
		schemas[schema.ID] = schema.SHA256
	}
	hello := actorprotocol.ActorHello{
		ActorID: "reviewer-1", Role: "reviewer", ProjectID: "project-a",
		AgentRunID: "run-1", ProtocolMajor: 2, ProtocolMinor: 0,
		Schemas: schemas, RoleBundleSHA256: bundle,
		Capabilities: []string{"review", "verdict.submit", "workflow.transition"},
	}
	required := []string{"flowbee.assignment/v2", "flowbee.result/v2"}
	got := c.Negotiate(hello, bundle, required)
	if !got.Compatible || got.GrantedProjectID != "project-a" || got.GrantedRole != "reviewer" {
		t.Fatalf("negotiation=%+v", got)
	}
	if strings.Join(got.Capabilities, ",") != "review,verdict.submit" {
		t.Fatalf("capability escalation was not filtered: %+v", got.Capabilities)
	}

	cases := []struct {
		name string
		edit func(*actorprotocol.ActorHello)
		want string
	}{
		{"major", func(h *actorprotocol.ActorHello) { h.ProtocolMajor = 3 }, "major mismatch"},
		{"new minor", func(h *actorprotocol.ActorHello) { h.ProtocolMinor = 1 }, "unsupported protocol minor"},
		{"bundle", func(h *actorprotocol.ActorHello) { h.RoleBundleSHA256 = "sha256:bad" }, "bundle mismatch"},
		{"schema", func(h *actorprotocol.ActorHello) { h.Schemas["flowbee.result/v2"] = "sha256:bad" }, "schema mismatch"},
		{"role", func(h *actorprotocol.ActorHello) { h.Role = "master" }, "unknown role"},
		{"scope", func(h *actorprotocol.ActorHello) { h.ProjectID = "" }, "project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copyHello := hello
			copyHello.Schemas = map[string]string{}
			for k, v := range hello.Schemas {
				copyHello.Schemas[k] = v
			}
			tc.edit(&copyHello)
			result := c.Negotiate(copyHello, bundle, required)
			if result.Compatible || !strings.Contains(result.Reason, tc.want) {
				t.Fatalf("negotiation=%+v want reason containing %q", result, tc.want)
			}
		})
	}
}
