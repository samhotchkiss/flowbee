package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	maxProviderStdoutBytes = 16 << 20
	maxProviderStderrBytes = 256 << 10
)

type agentCLIProvider struct{}

var claudeModelFlagRE = regexp.MustCompile(`--model(?:=|\s+)[^\s]+`)

func defaultProviderClients() map[string]providerClient {
	p := &agentCLIProvider{}
	return map[string]providerClient{
		"anthropic": p,
		"openai":    p,
	}
}

func (p *agentCLIProvider) Call(ctx context.Context, req ProviderRequest) (Response, error) {
	cmdReq, ok := req.Input.(AgentCommand)
	if !ok {
		return Response{}, fmt.Errorf("%w: llm.AgentCommand input required for %s", ErrInvalidRequest, req.Provider)
	}
	if strings.TrimSpace(cmdReq.Command) == "" {
		return Response{}, fmt.Errorf("%w: agent command is empty", ErrInvalidRequest)
	}
	ttl := cmdReq.TTLSeconds
	if ttl <= 0 {
		ttl = 300
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(ttl+30)*time.Second)
	defer cancel()

	command := applyResolvedAgentModel(cmdReq.Command, req.ModelID)
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = cmdReq.Dir
	cmd.Env = cmdReq.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second

	errb, outb := newBoundedWriter(maxProviderStderrBytes), newBoundedWriter(maxProviderStdoutBytes)
	cmd.Stderr = errb
	cmd.Stdout = outb
	if err := cmd.Start(); err != nil {
		return Response{}, fmt.Errorf("agent start: %w", err)
	}

	interval := agentHeartbeatIntervalS(ttl)
	done := make(chan struct{})
	if cmdReq.Heartbeat != nil {
		if !cmdReq.Heartbeat(runCtx) {
			cancel()
		}
		go func() {
			t := time.NewTicker(time.Duration(interval) * time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					if !cmdReq.Heartbeat(runCtx) {
						cancel()
						return
					}
				}
			}
		}()
	}

	werr := cmd.Wait()
	close(done)
	out := outb.String()
	usage := parseAgentUsage(out)
	if werr != nil {
		if runCtx.Err() != nil {
			return Response{Text: out, Usage: usage}, ProviderError{Code: "agent_aborted", Temporary: true, Message: fmt.Sprintf("agent aborted (lease revoked or exceeded lease_ttl): %v", runCtx.Err())}
		}
		return Response{Text: out, Usage: usage}, ProviderError{Code: "agent_cmd_failed", Message: fmt.Sprintf("agent cmd: %v: %s", werr, strings.TrimSpace(errb.String()))}
	}
	return Response{Text: out, Usage: usage}, nil
}

func applyResolvedAgentModel(command, modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" || !strings.HasPrefix(strings.TrimSpace(command), "claude ") {
		return command
	}
	replacement := "--model " + shellToken(modelID)
	if loc := claudeModelFlagRE.FindStringIndex(command); loc != nil {
		return command[:loc[0]] + replacement + command[loc[1]:]
	}
	return strings.TrimRight(command, " \t") + " " + replacement
}

func shellToken(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '/' || r == ':' ||
			(r >= '0' && r <= '9') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z'))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (p *agentCLIProvider) Embed(context.Context, ProviderRequest) (Response, error) {
	return Response{}, fmt.Errorf("%w: agent CLI provider does not support embeddings", ErrInvalidRequest)
}

func (p *agentCLIProvider) Stream(context.Context, ProviderRequest) (StreamHandle, error) {
	return nil, fmt.Errorf("%w: agent CLI provider does not support streaming", ErrInvalidRequest)
}

type boundedWriter struct {
	b   strings.Builder
	max int
}

func newBoundedWriter(max int) *boundedWriter { return &boundedWriter{max: max} }

func (w *boundedWriter) Write(p []byte) (int, error) {
	if rem := w.max - w.b.Len(); rem > 0 {
		if len(p) <= rem {
			w.b.Write(p)
		} else {
			w.b.Write(p[:rem])
		}
	}
	return len(p), nil
}

func (w *boundedWriter) String() string { return w.b.String() }

func parseAgentUsage(stdout string) Usage {
	s := strings.TrimSpace(stdout)
	if !strings.HasPrefix(s, "{") {
		return Usage{}
	}
	var r struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
		Usage        struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(s), &r) != nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:     r.Usage.InputTokens,
		CompletionTokens: r.Usage.OutputTokens,
		TotalTokens:      r.Usage.InputTokens + r.Usage.OutputTokens,
		EstimatedCostUSD: r.TotalCostUSD,
	}
}

func agentHeartbeatIntervalS(ttlS int) int {
	interval := ttlS / 3
	if interval > 60 {
		interval = 60
	}
	if interval < 20 {
		interval = 20
	}
	return interval
}
