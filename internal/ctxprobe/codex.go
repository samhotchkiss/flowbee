package ctxprobe

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// codexRolloutLine is the ALLOW-LIST parse of a rollout JSONL line's token_count
// payload — ONLY the context-window fields (total token usage + window size). No
// rate_limits, no identity, no token: ctxprobe carries zero credential surface.
type codexRolloutLine struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type string `json:"type"`
		Info struct {
			TotalTokenUsage struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"total_token_usage"`
			ModelContextWindow int64 `json:"model_context_window"`
		} `json:"info"`
	} `json:"payload"`
}

// ProbeCodex reads a Codex home's newest session context occupancy: the newest rollout
// log's LAST token_count event (total_token_usage.total_tokens vs model_context_window).
// Reading the tail yields the POST-compaction number after a mid-session summary (plan
// §15.3). Returns ok=false (no error) when no rollout with a token_count exists yet — a
// just-launched session has none, which is "unknown", not a failure.
func (p *Prober) ProbeCodex(codexHome string) (Reading, bool, error) {
	for _, path := range p.recentRolloutFiles(codexHome) {
		if r, ok := p.parseRolloutTail(path); ok {
			return r, true, nil
		}
	}
	return Reading{}, false, nil
}

// recentRolloutFiles returns up to maxRollouts rollout paths, newest first, walking
// sessions/<year>/<month>/<day>/ in descending order (zero-padded names sort lexically =
// chronologically). Mirrors acctprobe's walk so the two agree on "newest session".
func (p *Prober) recentRolloutFiles(dir string) []string {
	var out []string
	root := filepath.Join(dir, "sessions")
	for _, y := range p.descendingDirs(root) {
		for _, m := range p.descendingDirs(filepath.Join(root, y)) {
			for _, d := range p.descendingDirs(filepath.Join(root, y, m)) {
				dayDir := filepath.Join(root, y, m, d)
				entries, err := p.FS.ReadDir(dayDir)
				if err != nil {
					continue
				}
				var files []string
				for _, e := range entries {
					if !e.IsDir() && strings.HasPrefix(e.Name(), "rollout-") && strings.HasSuffix(e.Name(), ".jsonl") {
						files = append(files, e.Name())
					}
				}
				sort.Sort(sort.Reverse(sort.StringSlice(files)))
				for _, f := range files {
					out = append(out, filepath.Join(dayDir, f))
					if len(out) >= maxRollouts {
						return out
					}
				}
			}
		}
	}
	return out
}

func (p *Prober) descendingDirs(parent string) []string {
	entries, err := p.FS.ReadDir(parent)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	return dirs
}

// parseRolloutTail reads the tail of one rollout file and returns the LAST token_count
// event's context reading. ok=false when the tail held no usable token_count event.
func (p *Prober) parseRolloutTail(path string) (Reading, bool) {
	buf, ok := p.readTail(path)
	if !ok {
		return Reading{}, false
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !bytes.Contains(line, []byte(`"token_count"`)) {
			continue
		}
		var rl codexRolloutLine
		if json.Unmarshal(line, &rl) != nil || rl.Payload.Type != "token_count" {
			continue
		}
		// a token_count with no window is unusable for a percent; still report the
		// used-token count (window 0 = unknown) so a caller sees occupancy.
		return Reading{
			Provider:      ProviderCodex,
			UsedTokens:    rl.Payload.Info.TotalTokenUsage.TotalTokens,
			ContextWindow: rl.Payload.Info.ModelContextWindow,
			CapturedAt:    parseRFC3339(rl.Timestamp),
			Source:        path,
		}, true
	}
	return Reading{}, false
}
