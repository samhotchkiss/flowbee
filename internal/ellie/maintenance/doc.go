// Package maintenance is the shared production boundary for Ellie sweeps that
// can call an LLM.
//
// The repository does not carry separate legacy contradiction, dedup, reground,
// or reflection worker loops; the sweep packages in internal/ellie call through
// RunLLMSweep. That runner performs the durable completed-check lookup before
// invoking the judge and records only completed statuses after the judge result
// has been handled. Keep any future production sweep integration on this path so
// restarts, retries, and timer wakeups cannot reprocess unchanged content.
package maintenance
