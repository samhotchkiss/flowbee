// Package advisor is Rung E of the self-unblock ladder: a single-shot, READ-ONLY,
// no-tools model consult that NOMINATES one action for a job the deterministic janitor
// could not rescue. It is the ONLY LLM in the self-unblock path, reached rarely and capped
// hard; the store re-authorizes whatever it returns. It shells out to the SAME agent CLIs
// the fleet already runs (`claude -p` / `codex exec`) rather than embedding an API client,
// so it inherits the operator's existing auth + model selection. It is NOT a
// deterministic-core package (it does I/O); nothing in internal/{job,ledger,engine,
// liveness,scheduler} imports it.
package advisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Action is the CLOSED verdict set. Anything else (or a parse failure) is treated as Stop —
// the fail-safe: a broken or creative advisor can only ever leave a job parked, never
// loosen a gate.
type Action string

const (
	ActionPlan       Action = "PLAN"       // under-specified: note a concrete plan to try
	ActionCorrection Action = "CORRECTION" // prior attempt made a fixable mistake: note the fix
	ActionReprompt   Action = "REPROMPT"   // attempt stalled/flaked: note a tightened re-instruction
	ActionStop       Action = "STOP"       // no safe automatic retry — leave for a human
)

// Verdict is the advisor's decision plus a short note carried into the re-armed job's lease
// context (the fresh-context re-entry pack).
type Verdict struct {
	Action Action
	Note   string
}

// StuckJob is the read-only context handed to the advisor. It carries only what a human
// triaging the park would look at — no secrets, no tools, no repo write access.
type StuckJob struct {
	JobID           string
	Reason          string
	Kind            string
	HeadSHA         string
	Task            string
	Acceptance      string
	LastReviewNotes string
	LastCIFailures  string
	Attempts        int
	MaxAttempts     int
	UnblockAttempts int
}

// Advisor nominates an action for a stuck job.
type Advisor interface {
	Consult(ctx context.Context, j StuckJob) (Verdict, error)
}

// CLIAdvisor runs an agent CLI once, read-only, and parses a closed-set verdict. Cmd is a
// shell command run via `sh -c` that reads the prompt from $FLOWBEE_ADVISOR_PROMPT_FILE —
// mirroring the fleet's $FLOWBEE_TASK_FILE convention (see cmd/flowbee/fleet.go). Default
// is claude; set Cmd to the codex form for a codex box.
type CLIAdvisor struct {
	Cmd     string
	Timeout time.Duration
}

// DefaultClaudeCmd / DefaultCodexCmd read the prompt file and ask for a single JSON object.
// Read-only: no --dangerously-skip-permissions / file-writing brief (the builder templates
// add those); the advisor only needs to emit text.
const (
	DefaultClaudeCmd = `claude -p "$(cat "$FLOWBEE_ADVISOR_PROMPT_FILE")" --output-format json`
	DefaultCodexCmd  = `codex exec "$(cat "$FLOWBEE_ADVISOR_PROMPT_FILE")" --skip-git-repo-check < /dev/null`
)

// NewCLIAdvisor builds a CLIAdvisor, defaulting the command (claude) and a 90s timeout.
func NewCLIAdvisor(cmd string, timeout time.Duration) *CLIAdvisor {
	if strings.TrimSpace(cmd) == "" {
		cmd = DefaultClaudeCmd
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &CLIAdvisor{Cmd: cmd, Timeout: timeout}
}

// Consult runs the model once and returns its verdict. FAIL-SAFE: any exec failure, timeout,
// or unparseable output yields {Stop} with a non-nil error — the caller records the consult
// (so the model is not re-hammered) and leaves the job parked. It never returns a re-arm
// action it did not clearly parse.
func (a *CLIAdvisor) Consult(ctx context.Context, j StuckJob) (Verdict, error) {
	prompt := BuildPrompt(j)

	dir, err := os.MkdirTemp("", "flowbee-advisor-")
	if err != nil {
		return Verdict{Action: ActionStop}, fmt.Errorf("advisor tmpdir: %w", err)
	}
	defer os.RemoveAll(dir)
	pf := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(pf, []byte(prompt), 0o600); err != nil {
		return Verdict{Action: ActionStop}, fmt.Errorf("advisor prompt file: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", a.Cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FLOWBEE_ADVISOR_PROMPT_FILE="+pf)
	out, err := cmd.Output()
	if err != nil {
		return Verdict{Action: ActionStop}, fmt.Errorf("advisor exec: %w", err)
	}
	v, perr := ParseVerdict(string(out))
	if perr != nil {
		return Verdict{Action: ActionStop}, fmt.Errorf("advisor parse: %w", perr)
	}
	return v, nil
}

// BuildPrompt renders the read-only triage prompt. It constrains the model to a single JSON
// object over the closed action set and tells it to default to STOP when unsure.
func BuildPrompt(j StuckJob) string {
	var b strings.Builder
	b.WriteString("You are Flowbee's stuck-job advisor. A build/spec job is parked needing human help.\n")
	b.WriteString("Decide the ONE safest next action. You have NO tools and cannot edit code.\n\n")
	b.WriteString("Return ONLY a single-line JSON object, no prose, no markdown fences:\n")
	b.WriteString(`{"action":"PLAN|CORRECTION|REPROMPT|STOP","note":"<=240 chars, imperative"}` + "\n\n")
	b.WriteString("Actions:\n")
	b.WriteString("- PLAN: task under-specified; note a concrete plan/decomposition to try.\n")
	b.WriteString("- CORRECTION: prior attempt made a specific fixable mistake; note the correction.\n")
	b.WriteString("- REPROMPT: attempt was reasonable but stalled/flaked; note a tightened re-instruction.\n")
	b.WriteString("- STOP: no safe automatic retry — needs a human decision, external state, or you are unsure.\n\n")
	fmt.Fprintf(&b, "Job: id=%s reason=%s kind=%s attempts=%d/%d prior_auto_retries=%d\n",
		j.JobID, j.Reason, j.Kind, j.Attempts, j.MaxAttempts, j.UnblockAttempts)
	if j.Task != "" {
		fmt.Fprintf(&b, "Task: %s\n", trunc(j.Task, 1500))
	}
	if j.Acceptance != "" {
		fmt.Fprintf(&b, "Acceptance criteria: %s\n", trunc(j.Acceptance, 800))
	}
	if j.LastReviewNotes != "" {
		fmt.Fprintf(&b, "Prior review findings: %s\n", trunc(j.LastReviewNotes, 800))
	}
	if j.LastCIFailures != "" {
		fmt.Fprintf(&b, "Prior CI failures: %s\n", trunc(j.LastCIFailures, 400))
	}
	b.WriteString("\nIf you are not confident a retry will help, answer STOP. Default to STOP when uncertain.\n")
	return b.String()
}

// ParseVerdict extracts the closed-set verdict from an agent CLI's stdout. It first unwraps
// a `claude --output-format json` envelope (the answer text is its `.result`), then finds
// the verdict JSON object. An unrecognized action maps to Stop (fail-safe), never an error;
// only genuinely-absent JSON is an error.
func ParseVerdict(stdout string) (Verdict, error) {
	text := unwrapClaudeResult(stdout)
	obj, ok := firstJSONObject(text)
	if !ok {
		return Verdict{}, fmt.Errorf("no verdict json in output")
	}
	var raw struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := json.Unmarshal([]byte(obj), &raw); err != nil {
		return Verdict{}, fmt.Errorf("decode verdict: %w", err)
	}
	return Verdict{Action: normalizeAction(raw.Action), Note: strings.TrimSpace(raw.Note)}, nil
}

// unwrapClaudeResult returns the `.result` string of a claude JSON envelope, or the input
// unchanged if it is not one (a codex/plain-text agent prints the answer directly).
func unwrapClaudeResult(s string) string {
	s = strings.TrimSpace(s)
	var env struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(s), &env); err == nil && env.Result != "" {
		return env.Result
	}
	return s
}

// firstJSONObject returns the first balanced {…} run in s that parses as a JSON object with
// an "action" key. Tolerant of prose/markdown around the object.
func firstJSONObject(s string) (string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		depth, inStr, esc := 0, false, false
		for j := i; j < len(s); j++ {
			c := s[j]
			switch {
			case esc:
				esc = false
			case c == '\\' && inStr:
				esc = true
			case c == '"':
				inStr = !inStr
			case inStr:
				// skip
			case c == '{':
				depth++
			case c == '}':
				depth--
				if depth == 0 {
					cand := s[i : j+1]
					if strings.Contains(cand, `"action"`) {
						return cand, true
					}
					break
				}
			}
		}
	}
	return "", false
}

func normalizeAction(a string) Action {
	switch strings.ToUpper(strings.TrimSpace(a)) {
	case "PLAN":
		return ActionPlan
	case "CORRECTION":
		return ActionCorrection
	case "REPROMPT":
		return ActionReprompt
	default:
		return ActionStop // fail-safe: unknown/empty -> park for a human
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
