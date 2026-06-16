package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/samhotchkiss/flowbee/internal/auth"
)

// runToken mints a worker bearer token for an identity, signed by the control
// plane's FLOWBEE_WORKER_AUTH_SECRET (§7.6). Run it on the control plane (which
// holds the secret); copy the printed token into each remote worker's
// FLOWBEE_WORKER_TOKEN. The serve only accepts identities listed in its
// FLOWBEE_ENROLLED_IDENTITIES — a token for an un-enrolled identity is rejected.
//
//	FLOWBEE_WORKER_AUTH_SECRET=… flowbee token --identity feller-builder
func runToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	identity := fs.String("identity", "", "worker identity to mint a token for (must be enrolled on the serve)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *identity == "" {
		return fmt.Errorf("flowbee token: --identity is required")
	}
	secret := os.Getenv("FLOWBEE_WORKER_AUTH_SECRET")
	if secret == "" {
		return fmt.Errorf("flowbee token: set FLOWBEE_WORKER_AUTH_SECRET (the same secret the control plane runs with)")
	}
	// enrolled set is irrelevant for minting (it's enforced at Authenticate on the
	// serve); a nil allowlist is fine here.
	fmt.Println(auth.NewBearer([]byte(secret), nil, false).Mint(*identity))
	return nil
}
