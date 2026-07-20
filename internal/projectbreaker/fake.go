package projectbreaker

import (
	"context"
	"errors"
	"sync"
)

// FakeProbe is a deterministic dependency transport for control-plane and
// contract tests. Results are selected only by the stable project/repository
// scope; call order is retained for assertions. Missing fixtures fail closed.
type FakeProbe struct {
	mu      sync.Mutex
	Results map[string]ProbeResult
	Errors  map[string]error
	Calls   []ProbeRequest
}

func ProbeScopeKey(projectID, repoID string) string { return projectID + "\x00" + repoID }

func (p *FakeProbe) Probe(_ context.Context, req ProbeRequest) (ProbeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, req)
	key := ProbeScopeKey(req.ProjectID, req.RepoID)
	if err := p.Errors[key]; err != nil {
		return ProbeResult{}, err
	}
	result, ok := p.Results[key]
	if !ok {
		return ProbeResult{}, errors.New("project breaker fake: no fixture for scope")
	}
	return result, nil
}

func (p *FakeProbe) RecordedCalls() []ProbeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProbeRequest(nil), p.Calls...)
}
