# Flowbee × Elgato Stream Deck

A Stream Deck plugin that puts the Flowbee fleet on physical keys: per-account
usage gauges, goal-session (epic) state with one-tap jump to the tmux window,
master-session prompts, dispatch pause/resume, and attention alerts.

## The intended layout (15-key deck)

```
row 1   [claude:s]  [codex:gpt]  [claude:pearl]  [codex:s]   [ … ]      ← Account Usage
row 2   [epic-a]    [epic-b]     [epic-c]        [epic-d]    [ … ]      ← Goal Session
row 3   [master]    [ask status] [pause/resume]  [attention] [fleet]    ← controls
```

- **Account Usage** — Apple-Health-style concentric rings per agent account,
  one ring per limit window the provider actually reports, outer → inner:
  **session (5h, honey) / weekly (blue) / per-model "Fable" limit (violet)**.
  Claude accounts draw all three; Codex draws just the weekly ring while its
  session limit stays retired (it reappears automatically if the provider
  brings it back). The center shows the binding limit ("83% 5H"). The key
  **background turns yellow when session-or-week hits 80% and orange at 95%**
  (rings sit on a dark puck so they stay readable over the tint); a window at
  100%/critical renders red; everything dims when the last report is >24h old.
  Real per-window data arrives with the epic-lane digest API — until then the
  key shows a single gauge off today's `usage_pct`. Press → opens the fleet
  dashboard. On a Stream Deck + dial it renders as a `$B1` bar of the binding
  limit instead.
- **Goal Session** — one goal session / epic per key (`GET /v1/sessions`, the
  watchdog registry): PURSUING / WORKING / **BLOCKED (flashes red)** / PARKED /
  DONE / UNREACHABLE, with elapsed time or the auto-resume gate ("→ 10:47") as
  the footer. **Press → jumps to that tmux window** (selects the exact iTerm
  tab attached to it, or opens a fresh attach; `ssh -t` attach for sessions on
  another box). Local tmux sessions that aren't registered yet appear as
  UNWATCHED — register them with `flowbee session add <id> --tmux <name>` to
  get real states.
- **Go to Master** — jump to your master/planning session's window.
- **Prompt Master** — types a configured prompt into the master session (text,
  then Enter as a separate keystroke, exactly like the Flowbee watchdog does)
  and jumps to it. Default prompt: *"Give me the current status of all of our
  goals"*. Drop several of these on the deck with different prompts to build a
  prompt palette.
- **Pause / Resume** — toggles dispatch (`POST /v1/control/pause|resume`);
  set a repo id in the key's settings to park just that repo instead.
- **Attention** — needs-human + merge-handoff + needs-input count; flashes red
  when anything is waiting on you. Press → dashboard.
- **Fleet Health** — live/stale workers + waiting jobs; flashes STRANDED when
  work is queued with zero live workers.

Keys auto-assign by **column**: five Account Usage keys dropped across row 1
show accounts 1–5 in the server's stable order; same for Goal Session keys in
row 2. Pin a specific account/session in the key's property inspector instead
whenever you want a fixed assignment.

## Install

```bash
cd addons/streamdeck
npm install
npm run build
npx @elgato/cli link ./com.samhotchkiss.flowbee.sdPlugin   # symlink into Stream Deck
# then restart the Stream Deck app once, or: npx @elgato/cli restart com.samhotchkiss.flowbee
```

Settings live in any key's property inspector (shared across all keys):
Flowbee URL (default `http://127.0.0.1:7070`), optional API token (only needed
off-loopback — mint with `flowbee token`), the master tmux session name, the
terminal app (iTerm2 / Terminal), and the poll interval.

macOS will ask once to let Stream Deck control iTerm2/Terminal (Automation
permission) the first time you press a session key.

## How it talks to Flowbee

Read endpoints (`/v1/fleet`, `/v1/sessions`, `/v1/fleet-health`,
`/v1/needs-human`, `/v1/merge-handoff`, `/v1/needs-input`) are polled per
resource — only while a key that needs them is visible — with the SSE feed
(`/v1/events`) as a latency nudge: `control`/`capacity` events refresh their
resource immediately, job-lifecycle events debounce an attention refresh. The
SSE stream is lossy by design, so polling remains the source of truth.
`GET /v1/sessions` is feature-detected: on a 404 (older control plane) the
plugin falls back to plain `tmux list-sessions`, so row 2 still works — just
without watchdog states. When the epic-lane digest API lands (Phase 6/7),
`src/flowbee/service.ts` resource fetchers are the only swap point.

## Development

```bash
npm run watch     # rebuild + restart the plugin on change
```

Logs: `com.samhotchkiss.flowbee.sdPlugin/logs/`. Property-inspector debugging:
`npx @elgato/cli dev`, then open http://localhost:23654/. To attach a Node
debugger to the plugin itself, temporarily add `"Debug": "enabled"` under
`Nodejs` in manifest.json — it is deliberately off in the shipped manifest
(an open inspector port on a process that can exec tmux/ssh/osascript is a
local-RCE foothold).

Security posture: the API token is only ever attached when the base URL is
loopback or https — a typo'd off-box `http://` host gets no credentials (a
warning is logged instead).
