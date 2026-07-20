package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const bootstrapAutomationTokenFileEnv = "FLOWBEE_HUMAN_AUTOMATION_TOKEN_FILE"

// bootstrapAPIClient is explicit about the dashboard trust boundary. The bare
// CLI uses an owner-only automation bearer file whose credential-bound identity
// must also have the required HumanAccess grant. Cookie+CSRF remains available
// for browser-contract tests, but the two origins may never be mixed.
type bootstrapAPIClient struct {
	BaseURL, Bearer, SessionCookie, CSRFToken string
	Client                                    *http.Client
}

func bootstrapAPIClientFromConfiguredBearer(baseURL string, client *http.Client) (bootstrapAPIClient, error) {
	path := strings.TrimSpace(os.Getenv(bootstrapAutomationTokenFileEnv))
	if path == "" {
		return bootstrapAPIClient{}, fmt.Errorf("%s must name the owner-only automation bearer file", bootstrapAutomationTokenFileEnv)
	}
	if !filepath.IsAbs(path) {
		return bootstrapAPIClient{}, fmt.Errorf("%s must be an absolute path", bootstrapAutomationTokenFileEnv)
	}
	token, err := readOwnerOnlySecret(path)
	if err != nil {
		return bootstrapAPIClient{}, fmt.Errorf("read bootstrap automation bearer: %w", err)
	}
	return bootstrapAPIClient{BaseURL: baseURL, Bearer: token, Client: client}, nil
}

func (c bootstrapAPIClient) Commit(ctx context.Context, action api.BootstrapAction) (api.BootstrapActionReceipt, error) {
	viaBearer, _, ok := c.authModes()
	if !ok {
		return api.BootstrapActionReceipt{}, errors.New("authenticated bootstrap API client is incomplete")
	}
	body, err := json.Marshal(action)
	if err != nil {
		return api.BootstrapActionReceipt{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/v1/bootstrap/actions", bytes.NewReader(body))
	if err != nil {
		return api.BootstrapActionReceipt{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", action.ActionID)
	if viaBearer {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	} else {
		req.Header.Set("X-Flowbee-CSRF", c.CSRFToken)
		req.Header.Set("Cookie", c.SessionCookie)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return api.BootstrapActionReceipt{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return api.BootstrapActionReceipt{}, fmt.Errorf("bootstrap API status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var receipt api.BootstrapActionReceipt
	if err := decodeBootstrapAPIJSON(io.LimitReader(resp.Body, 64<<10), &receipt); err != nil {
		return api.BootstrapActionReceipt{}, err
	}
	if receipt.FormatVersion != bootstrapActionReceiptFormat || receipt.ActionID != action.ActionID ||
		receipt.ReceiptID == "" || !validBootstrapActionState(receipt.State) {
		return api.BootstrapActionReceipt{}, errors.New("bootstrap API returned mismatched receipt")
	}
	return receipt, nil
}

func (c bootstrapAPIClient) Status(ctx context.Context, actionID string) (api.BootstrapActionStatus, error) {
	viaBearer, viaBrowser, ok := c.authModes()
	if !ok || strings.TrimSpace(actionID) == "" || strings.Contains(actionID, "/") {
		return api.BootstrapActionStatus{}, errors.New("authenticated bootstrap API status request is incomplete")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.BaseURL, "/")+"/v1/bootstrap/actions/"+actionID, nil)
	if err != nil {
		return api.BootstrapActionStatus{}, err
	}
	if viaBearer {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	} else if viaBrowser {
		req.Header.Set("X-Flowbee-CSRF", c.CSRFToken)
		req.Header.Set("Cookie", c.SessionCookie)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return api.BootstrapActionStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return api.BootstrapActionStatus{}, fmt.Errorf("bootstrap status API status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var status api.BootstrapActionStatus
	if err := decodeBootstrapAPIJSON(io.LimitReader(resp.Body, 64<<10), &status); err != nil {
		return api.BootstrapActionStatus{}, err
	}
	if status.FormatVersion != "flowbee.bootstrap-action-status/v1" || status.ActionID != actionID ||
		status.ProjectID == "" || !validBootstrapActionState(status.State) {
		return api.BootstrapActionStatus{}, errors.New("bootstrap status API returned mismatched status")
	}
	return status, nil
}

func (c bootstrapAPIClient) Activation(ctx context.Context, projectID string) (store.ProjectActivationStatus, error) {
	viaBearer, viaBrowser, ok := c.authModes()
	if !ok || strings.TrimSpace(projectID) == "" || strings.Contains(projectID, "/") {
		return store.ProjectActivationStatus{}, errors.New("authenticated bootstrap activation request is incomplete")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.BaseURL, "/")+"/v1/projects/"+projectID+"/activation", nil)
	if err != nil {
		return store.ProjectActivationStatus{}, err
	}
	if viaBearer {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	} else if viaBrowser {
		req.Header.Set("X-Flowbee-CSRF", c.CSRFToken)
		req.Header.Set("Cookie", c.SessionCookie)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return store.ProjectActivationStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return store.ProjectActivationStatus{}, fmt.Errorf("bootstrap activation API status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var envelope struct {
		SchemaVersion string                        `json:"schema_version"`
		Activation    store.ProjectActivationStatus `json:"activation"`
	}
	if err := decodeBootstrapAPIJSON(io.LimitReader(resp.Body, 256<<10), &envelope); err != nil {
		return store.ProjectActivationStatus{}, err
	}
	if envelope.SchemaVersion != "flowbee.project-activation/v1" || envelope.Activation.Project.ID != projectID {
		return store.ProjectActivationStatus{}, errors.New("bootstrap activation API returned mismatched project")
	}
	return envelope.Activation, nil
}

func decodeBootstrapAPIJSON(r io.Reader, value any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("bootstrap API response contains trailing or malformed JSON")
	}
	return nil
}

func validBootstrapActionState(state string) bool {
	switch state {
	case "pending", "claimed", "verifying", "succeeded", "uncertain", "held", "dead_letter":
		return true
	default:
		return false
	}
}

func (c bootstrapAPIClient) authModes() (viaBearer, viaBrowser, ok bool) {
	viaBearer = c.Bearer != "" && c.SessionCookie == "" && c.CSRFToken == ""
	viaBrowser = c.Bearer == "" && c.SessionCookie != "" && c.CSRFToken != ""
	return viaBearer, viaBrowser, c.BaseURL != "" && c.Client != nil && (viaBearer || viaBrowser)
}
