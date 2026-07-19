package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/auth"
)

func configuredHumanAccess(privateAddr string, automation auth.Authenticator,
	phase1Enabled bool) (*auth.HumanAccess, error) {
	keyFile := strings.TrimSpace(os.Getenv("FLOWBEE_HUMAN_SESSION_KEY_FILE"))
	grantsFile := strings.TrimSpace(os.Getenv("FLOWBEE_HUMAN_GRANTS_FILE"))
	loopbackDev := os.Getenv("FLOWBEE_HUMAN_LOOPBACK_DEV") == "1"
	if keyFile == "" || grantsFile == "" {
		if keyFile != "" || grantsFile != "" {
			return nil, errors.New("FLOWBEE_HUMAN_SESSION_KEY_FILE and FLOWBEE_HUMAN_GRANTS_FILE must be configured together")
		}
		if loopbackDev && isLoopbackAddr(privateAddr) {
			return auth.NewHumanAccess(nil, automation, nil, true), nil
		}
		if phase1Enabled || !isLoopbackAddr(privateAddr) {
			return nil, errors.New("dashboard human access requires owner-only FLOWBEE_HUMAN_SESSION_KEY_FILE and FLOWBEE_HUMAN_GRANTS_FILE (or explicit loopback-only FLOWBEE_HUMAN_LOOPBACK_DEV=1)")
		}
		// Phase 1 is disabled and the listener is loopback-only. Keep the old
		// development posture without accidentally widening it onto the Tailnet.
		return auth.NewHumanAccess(nil, automation, nil, true), nil
	}
	secret, err := readOwnerOnlySecret(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read human session signing key: %w", err)
	}
	if len([]byte(secret)) < 32 {
		return nil, errors.New("human session signing key must contain at least 32 bytes")
	}
	raw, err := readOwnerOnlySecret(grantsFile)
	if err != nil {
		return nil, fmt.Errorf("read human grants: %w", err)
	}
	entries := strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == ',' })
	grants, err := auth.ParseHumanGrants(entries)
	if err != nil {
		return nil, fmt.Errorf("decode human grants: %w", err)
	}
	if len(grants) == 0 {
		return nil, errors.New("human grants file must enroll at least one identity")
	}
	return auth.NewHumanAccess([]byte(secret), automation, grants, false), nil
}
