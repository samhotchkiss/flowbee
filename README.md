<div align="center">

# 🐝 Flowbee

### An orchestrator for fleets of AI coding agents.
**Bring your own models. Bring your own hardware.**

![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![Store](https://img.shields.io/badge/store-SQLite-003B57?logo=sqlite&logoColor=white)
![Deploy](https://img.shields.io/badge/deploy-single%20static%20binary-success)
![License](https://img.shields.io/badge/license-MIT-blue)
![Status](https://img.shields.io/badge/status-early%20%26%20moving%20fast-orange)

</div>

---

Point a GitHub repo at a pool of machines running coding agents — Codex, Claude, whatever you've got — and Flowbee turns them into an engineering org. Give it a goal; it breaks the work into issues, reviews the plan, builds each piece, reviews the build, and merges it.

**You bring the agents and the boxes. Flowbee runs the line.**

One static binary. No Postgres, no Docker, no cloud. `flowbee serve` and you're orchestrating a fleet.

## The idea

> *"Hi, I'm codex — got a job for me?"*

Workers are thin, self-identifying pull-loops. They dial **out** to Flowbee, lease a job, do it with whatever agent they wrap, and report back. **They never touch GitHub.** Flowbee is the brain — and the only thing that talks to GitHub.

```
  you ⇄ your agent ──▶  EPIC { goal, issues + deps }
                          │
   ┌──────────────────────┴─────────────────────────────────────────┐
   │  issue review (whole epic) ─▶ build ─▶ build review ×N ─▶ merge   │
   └──────────────────────────────────────────────────────────────────┘
        Flowbee leases each step to a capable, idle agent, drives it
        build → review → merge, and syncs state to GitHub as it goes.
```

## Why it's different

🧠&nbsp; **The control plane has no LLM.** Flowbee itself is deterministic and replayable — it never hallucinates, it's unit-tested, and its entire history folds from an event log. The intelligence lives in the agents, at the edges.

🔌&nbsp; **Provider-agnostic.** Codex builds, Opus reviews, a local model runs the tests — roles are config, swapped freely. Nothing is welded to one vendor.

🪪&nbsp; **Identity-enforced.** Every actor has an identity and a lens. A reviewer can't approve its own build; a same-family reviewer can't rubber-stamp a shared blind spot. **Flowbee — not GitHub — is the authority on who did what.**

📓&nbsp; **System of record.** Flowbee owns the process: the job graph, every verdict, the full lineage from chat to merge. A GitHub issue or PR is just a *rendering* of a Flowbee job.

🎯&nbsp; **No more arch-lottery.** Jobs carry constraints derived from the diff; workers advertise verified capabilities. The E2E suite runs on the machine it's meant to.

🪶&nbsp; **Lightweight by obsession.** One static Go binary, a single SQLite file, zero services. Laptop, homelab, or rack — same binary, over LAN or Tailscale.

🔁&nbsp; **Flows are config.** Drop the issue-review stage. Add three build-reviewers that each test something different. It's a YAML file.

## Quickstart

```bash
# one static binary, zero dependencies
go install github.com/samhotchkiss/flowbee/cmd/flowbee@latest

# scaffold config into your repo (committed + versioned)
cd my-project && flowbee init

# give it access, then run
export FLOWBEE_GITHUB_TOKEN=github_pat_...    # repo-scoped, fine-grained
flowbee serve &       # the control plane
flowbee work  &       # a worker — or /loop a Claude session as one

flowbee doctor        # ✅ green? you're live.
```

Submit an epic, then watch the board at **localhost:7070**.

## Under the hood

- **Two domains.** Flowbee is system-of-record for the *process*; GitHub is ground truth only for the facts it owns (PR exists, CI status, merged). Two loops: reconcile **in**, project **out**.
- **Fenced, exactly-once leases.** Each job goes to exactly one worker, fenced by an epoch so a zombie can't clobber a reassignment.
- **Trust nothing, verify everything.** Verdicts derive from reconciled facts, never a worker's say-so. Liveness detection tells *slow* from *stuck*. A content-integrity gate keeps a prompt-injected diff out of `main`.
- **Compounding memory.** Every issue and outcome is archived in-repo, so the fleet stops re-deriving dead ends and re-submitting patches that already failed review.

Go deeper: **[DESIGN.md](./DESIGN.md)** — the full architecture · **[BUILD.md](./BUILD.md)** — the milestone plan.

## Status

Early, and moving fast — the control plane, fenced leasing, the flow engine, GitHub sync, and liveness are landing milestone by milestone. Built, fittingly, by a fleet of agents.

## License

MIT © Sam Hotchkiss
