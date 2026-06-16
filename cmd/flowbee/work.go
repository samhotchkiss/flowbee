package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runWork runs the Mode-A harness. In M1, --stub runs the echo stub loop once.
func runWork(args []string) error {
	fs := flag.NewFlagSet("work", flag.ContinueOnError)
	stub := fs.Bool("stub", false, "run the built-in echo stub worker (M1)")
	identity := fs.String("identity", envOr("FLOWBEE_IDENTITY", "stub-worker"), "worker identity")
	family := fs.String("model-family", envOr("FLOWBEE_MODEL_TAG", "stub"), "model family tag")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*stub {
		return fmt.Errorf("only --stub is implemented in M1")
	}
	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	out, err := worker.RunOnce(context.Background(), worker.StubConfig{
		BaseURL: url, Identity: *identity, ModelFamily: *family,
	})
	if err != nil {
		return err
	}
	if !out.Got {
		fmt.Println("no work available (204)")
		return nil
	}
	fmt.Printf("stub completed job %s -> %s (epoch %d)\n", out.JobID, out.JobState, out.LeaseEpoch)
	return nil
}

// runLease is the Mode-B thin client: one GET /v1/lease, print JSON.
func runLease(args []string) error {
	fs := flag.NewFlagSet("lease", flag.ContinueOnError)
	identity := fs.String("identity", envOr("FLOWBEE_IDENTITY", "modeb"), "worker identity")
	family := fs.String("model-family", envOr("FLOWBEE_MODEL_TAG", "stub"), "model family tag")
	role := fs.String("role", "", "role filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	c := client.New(url)
	grant, ok, err := c.Lease(context.Background(), *identity, *family, *role)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println(`{"lease":null}`)
		return nil
	}
	b, _ := json.Marshal(grant)
	fmt.Println(string(b))
	return nil
}

// runSubmit is the Mode-B thin client: post a result (or heartbeat/release).
func runSubmit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	jobID := fs.String("job", "", "job id")
	epoch := fs.Int("epoch", 0, "lease epoch (the fence)")
	action := fs.String("action", "result", "result|heartbeat|release")
	idem := fs.String("idempotency-key", "", "idempotency key (result only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jobID == "" {
		return fmt.Errorf("--job is required")
	}
	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	c := client.New(url)
	ctx := context.Background()

	switch *action {
	case "heartbeat":
		dir, st, err := c.Heartbeat(ctx, *jobID, *epoch)
		if err != nil {
			return err
		}
		fmt.Printf("status=%d directive=%s\n", st, dir)
	case "release":
		st, err := c.Release(ctx, *jobID, *epoch)
		if err != nil {
			return err
		}
		fmt.Printf("status=%d\n", st)
	case "result":
		body := map[string]any{"kind": "patch", "blast_radius": map[string]any{"scope": "modeb"}}
		res, st, err := c.Result(ctx, *jobID, *epoch, *idem, body)
		if err != nil {
			return err
		}
		fmt.Printf("status=%d accepted=%v job_state=%s\n", st, res.Accepted, res.JobState)
	default:
		return fmt.Errorf("unknown action %q", *action)
	}
	return nil
}
