package actorprotocol

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const generatedHeader = "<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->\n"

// GeneratedFiles derives the bounded actor cards, recovery runbooks, and dashboard
// label registry from the normative contract. Paths are repository-relative and
// stable so CI can compare them byte-for-byte.
func GeneratedFiles(c Contract) (map[string][]byte, error) {
	hash, err := BundleHash()
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, role := range c.Roles {
		var b strings.Builder
		b.WriteString(generatedHeader)
		fmt.Fprintf(&b, "# %s role card\n\nProtocol `%s` · bundle `%s`\n\n", title(role.ID), c.Version(), hash)
		fmt.Fprintf(&b, "## Authority\n\n%s\n\n", role.Authority)
		writeList(&b, "Required outputs", role.Outputs)
		writeList(&b, "Capabilities", role.Capabilities)
		writeList(&b, "Autonomous recovery", role.Recovery)
		writeList(&b, "Escalates to", role.EscalatesTo)
		writeList(&b, "Forbidden", role.Forbidden)
		files[filepath.Join("docs/runbooks/actors", role.ID+".md")] = []byte(b.String())
	}

	labels := map[string]map[string]string{}
	for _, recovery := range c.RecoveryCodes {
		var b strings.Builder
		b.WriteString(generatedHeader)
		fmt.Fprintf(&b, "# `%s`\n\nProtocol `%s` · bundle `%s`\n\n", recovery.Code, c.Version(), hash)
		fmt.Fprintf(&b, "- Severity: `%s`\n- Owner: `%s`\n- Escalates to: `%s`\n- Automatic: `%t`\n- Maximum automatic attempts: `%d`\n- Named proof: `%s`\n\n", recovery.Severity, recovery.Owner, recovery.EscalationTarget, recovery.Automatic, recovery.MaximumAttempts, recovery.TestID)
		fmt.Fprintf(&b, "## Invariant\n\n%s\n\n## Authoritative predicate\n\n%s\n\n## Repair\n\nRun the domain action `%s` under fence `%s`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.\n", recovery.Invariant, recovery.Predicate, recovery.RepairAction, recovery.Fence)
		files[filepath.Join("docs/runbooks/recovery", recovery.Code+".md")] = []byte(b.String())
		labels[recovery.Code] = map[string]string{
			"severity":       recovery.Severity,
			"help_path":      recovery.HelpPath,
			"owner":          recovery.Owner,
			"attention_kind": recovery.AttentionKind,
		}
	}
	labelJSON, err := json.MarshalIndent(labels, "", "  ")
	if err != nil {
		return nil, err
	}
	labelJSON = append(labelJSON, '\n')
	files["internal/web/assets/recovery-labels.generated.json"] = labelJSON
	return files, nil
}

func writeList(b *strings.Builder, heading string, values []string) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n", heading)
	for _, value := range values {
		fmt.Fprintf(b, "- %s\n", value)
	}
	b.WriteByte('\n')
}

func title(id string) string {
	parts := strings.Split(id, "_")
	for i := range parts {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, " ")
}

func SortedGeneratedPaths(files map[string][]byte) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
