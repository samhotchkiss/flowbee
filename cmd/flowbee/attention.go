package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// runAttention is the `flowbee attention <list>` CLI (epic-lane Phase 6b) — the operator's
// read-only view of the durable attention queue (plan §1.3/§1.4). It hits the open-tier
// GET /v1/masters/attention read endpoint, so it needs no master registration; masters
// LEASE + resolve via `flowbee master`, operators eyeball via this.
func runAttention(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flowbee attention <list> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runAttentionList(rest)
	default:
		return fmt.Errorf("unknown `flowbee attention` subcommand %q (want list)", sub)
	}
}

func runAttentionList(args []string) error {
	fs := flag.NewFlagSet("attention list", flag.ContinueOnError)
	state := fs.String("state", "", "filter to a single state (open|leased|delivering|awaiting_ack); default = all active")
	kinds := fs.String("kinds", "", "comma-separated kinds to filter to (e.g. needs_input,scope_violation)")
	asJSON := fs.Bool("json", false, "print the raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := "/v1/masters/attention"
	q := []string{}
	if *state != "" {
		q = append(q, "state="+*state)
	}
	if *kinds != "" {
		q = append(q, "kinds="+*kinds)
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	var resp struct {
		DigestSeq int64            `json:"digest_seq"`
		Items     []map[string]any `json:"items"`
	}
	raw, err := masterGet(path, &resp)
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	if len(resp.Items) == 0 {
		fmt.Printf("no open attention items (digest_seq=%d)\n", resp.DigestSeq)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tPRIO\tSTATE\tEPIC\tDEDUP\tDETAIL")
	for _, it := range resp.Items {
		fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v\t%v\t%v\n",
			it["id"], it["kind"], it["priority"], it["state"], dashIfEmpty(str(it["epic"])),
			dashIfEmpty(str(it["dedup_key"])), dashIfEmpty(truncate(str(it["detail"]), 40)))
	}
	tw.Flush() //nolint:errcheck
	fmt.Printf("\n%d open item(s) (digest_seq=%d)\n", len(resp.Items), resp.DigestSeq)
	return nil
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
