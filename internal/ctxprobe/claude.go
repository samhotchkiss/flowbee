package ctxprobe

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// claudeTranscriptLine is the ALLOW-LIST parse of a Claude Code transcript JSONL line.
// The assistant turn's message.usage carries the context occupancy; NO token or secret
// field is decoded. ContextWindow is read ONLY if the transcript happens to carry one
// on disk (see contextWindowOf) — it is NEVER guessed from the model name (plan §12.4).
type claudeTranscriptLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Model string       `json:"model"`
		Usage *claudeUsage `json:"usage"`
	} `json:"message"`
	// Some Claude Code builds stamp a window on the line/usage; both spellings are
	// accepted, and their ABSENCE yields an unknown window (never a fabricated one).
	ContextWindow      int64 `json:"context_window"`
	ModelContextWindow int64 `json:"model_context_window"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	// window, if a build ever records it inside usage.
	ContextWindow      int64 `json:"context_window"`
	ModelContextWindow int64 `json:"model_context_window"`
}

// occupied returns the context tokens the turn READ (prompt + both cache tiers) — the
// current occupancy of the window. output_tokens are generated, not resident context.
func (u claudeUsage) occupied() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// contextWindowOf returns the window size a transcript line carried on disk, or 0 when
// absent. Checked at both the line and usage level; NEVER inferred from message.model.
func contextWindowOf(l claudeTranscriptLine) int64 {
	for _, v := range []int64{l.ContextWindow, l.ModelContextWindow} {
		if v > 0 {
			return v
		}
	}
	if l.Message.Usage != nil {
		for _, v := range []int64{l.Message.Usage.ContextWindow, l.Message.Usage.ModelContextWindow} {
			if v > 0 {
				return v
			}
		}
	}
	return 0
}

// ProbeClaude reads a Claude session's context occupancy from its per-cwd transcript:
// config_dir/projects/<cwd-slug>/<newest>.jsonl, taking the LAST assistant message's
// usage. cwd is the session's working directory (the epic checkout); its slug is Claude
// Code's own encoding (every non-alphanumeric rune -> '-'). Returns ok=false (no error)
// when the project dir / a usable assistant-usage line is absent. The context WINDOW is
// returned only if present on disk; otherwise ContextWindow=0 (RemainingPct ok=false) —
// ctxprobe NEVER guesses the window from the model name (plan §12.4).
func (p *Prober) ProbeClaude(configDir, cwd string) (Reading, bool, error) {
	projectDir := filepath.Join(configDir, "projects", SlugForCwd(cwd))
	path, ok := p.newestTranscript(projectDir)
	if !ok {
		return Reading{}, false, nil
	}
	buf, ok := p.readTail(path)
	if !ok {
		return Reading{}, false, nil
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var tl claudeTranscriptLine
		if json.Unmarshal(line, &tl) != nil || tl.Message.Usage == nil {
			continue
		}
		if tl.Message.Usage.occupied() == 0 {
			continue // a usage line with no input tokens tells us nothing about occupancy
		}
		return Reading{
			Provider:      ProviderClaude,
			UsedTokens:    tl.Message.Usage.occupied(),
			ContextWindow: contextWindowOf(tl), // 0 = unknown; never guessed
			CapturedAt:    parseRFC3339(tl.Timestamp),
			Source:        path,
		}, true, nil
	}
	return Reading{}, false, nil
}

// SlugForCwd encodes a working directory the way Claude Code names its per-project
// transcript directory: every rune that is not ASCII alphanumeric becomes '-'. So
// "/Users/sam/dev/flowbee" -> "-Users-sam-dev-flowbee". Exposed so the caller can name
// the same directory when wiring the probe.
func SlugForCwd(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// newestTranscript returns the newest *.jsonl in dir (by mtime, ties broken by name
// descending for determinism), or ok=false when the dir is absent/empty.
func (p *Prober) newestTranscript(dir string) (string, bool) {
	entries, err := p.FS.ReadDir(dir)
	if err != nil {
		return "", false
	}
	type cand struct {
		name string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		mod := time.Time{}
		if info, ierr := e.Info(); ierr == nil {
			mod = info.ModTime()
		}
		cands = append(cands, cand{name: e.Name(), mod: mod})
	}
	if len(cands) == 0 {
		return "", false
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].mod.Equal(cands[j].mod) {
			return cands[i].mod.After(cands[j].mod)
		}
		return cands[i].name > cands[j].name
	})
	return filepath.Join(dir, cands[0].name), true
}
