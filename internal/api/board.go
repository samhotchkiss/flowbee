package api

import (
	"html/template"
	"net/http"

	"github.com/samhotchkiss/flowbee/internal/store"
)

var boardTmpl = template.Must(template.New("board").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Flowbee board</title>
<style>body{font:14px system-ui;margin:2rem}table{border-collapse:collapse}
td,th{border:1px solid #ccc;padding:4px 8px}th{background:#f3f3f3}</style></head>
<body><h1>Flowbee board</h1>
<table><tr><th>job</th><th>kind</th><th>state</th><th>role</th><th>epoch</th><th>identity</th></tr>
{{range .}}<tr><td>{{.ID}}</td><td>{{.Kind}}</td><td>{{.State}}</td><td>{{.Role}}</td><td>{{.LeaseEpoch}}</td><td>{{.Identity}}</td></tr>
{{end}}</table>
<p>Live feed at <code>/v1/events</code>.</p></body></html>`))

func renderBoard(w http.ResponseWriter, jobs []store.BoardJob) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = boardTmpl.Execute(w, jobs)
}

// rosterTmpl renders the worker roster (DESIGN §12.6.2): who's connected, on
// what, where, with the active lease and a ⚠ stale-hb badge for a partitioned
// worker (last heartbeat older than the threshold).
var rosterTmpl = template.Must(template.New("roster").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Flowbee roster</title>
<style>body{font:14px system-ui;margin:2rem}table{border-collapse:collapse}
td,th{border:1px solid #ccc;padding:4px 8px}th{background:#f3f3f3}
.stale{color:#b00;font-weight:600}</style></head>
<body><h1>Flowbee worker roster</h1>
<table><tr><th>worker</th><th>identity</th><th>host</th><th>arch/os</th>
<th>attested caps</th><th>lease</th><th>last hb</th></tr>
{{range .}}<tr>
<td>{{.WorkerID}}</td><td>{{.Identity}}</td><td>{{.Host}}</td>
<td>{{.Arch}}/{{.OS}}</td>
<td>{{range .Attested}}{{.}} {{end}}</td>
<td>{{if .ActiveJob}}{{.ActiveJob}}/e{{.ActiveEpoch}}{{else}}—{{end}}</td>
<td>{{.LastSeenAgo}}s ago{{if .StaleHB}} <span class="stale">⚠ stale-hb</span>{{end}}</td>
</tr>
{{end}}</table>
<p>Board at <code>/</code>; JSON at <code>/v1/roster</code>.</p></body></html>`))

func renderRoster(w http.ResponseWriter, workers []store.RosterWorker) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = rosterTmpl.Execute(w, workers)
}

// dashboardData is the unified operator UI payload (DESIGN §12.6): board +
// roster + budget + cost + audit + the needs_human chokepoint, on one live page.
type dashboardData struct {
	Board      []store.BoardJob
	Roster     []store.RosterWorker
	Budget     store.RateLimitGauge
	Cost       []store.FlowCostRow
	Audit      []store.AuditRow
	NeedsHuman []store.NeedsHumanRow
}

// dashboardTmpl renders the finished UI (§12.6): board / roster / budget / audit
// / cost, refreshed live off the SSE feed at /v1/events (any lifecycle event
// reloads the panes). One page, read-only, off the SQL views.
var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Flowbee dashboard</title>
<style>body{font:14px system-ui;margin:1.5rem;max-width:1100px}
h1{margin-bottom:.2rem}h2{margin-top:1.6rem}
table{border-collapse:collapse;width:100%}
td,th{border:1px solid #ccc;padding:3px 7px;text-align:left}th{background:#f3f3f3}
.stale,.over{color:#b00;font-weight:600}.gauge{font-weight:600}
.panes{display:grid;grid-template-columns:1fr 1fr;gap:1.2rem}</style></head>
<body>
<h1>Flowbee dashboard</h1>
<p class="gauge">GitHub budget: {{.Budget.Remaining}} / {{.Budget.Limit}} remaining
(last sweep {{.Budget.LastSweep.Format "15:04:05"}})</p>

<h2>Board</h2>
<table><tr><th>job</th><th>kind</th><th>state</th><th>role</th><th>epoch</th><th>identity</th></tr>
{{range .Board}}<tr><td>{{.ID}}</td><td>{{.Kind}}</td><td>{{.State}}</td><td>{{.Role}}</td><td>{{.LeaseEpoch}}</td><td>{{.Identity}}</td></tr>
{{end}}</table>

<div class="panes">
<div>
<h2>Roster</h2>
<table><tr><th>identity</th><th>host</th><th>lease</th><th>last hb</th></tr>
{{range .Roster}}<tr><td>{{.Identity}}</td><td>{{.Host}}</td>
<td>{{if .ActiveJob}}{{.ActiveJob}}/e{{.ActiveEpoch}}{{else}}—{{end}}</td>
<td>{{.LastSeenAgo}}s ago{{if .StaleHB}} <span class="stale">⚠ stale-hb</span>{{end}}</td></tr>
{{end}}</table>
</div>
<div>
<h2>needs_human</h2>
<table><tr><th>job</th><th>flow</th><th>role</th><th>reason</th></tr>
{{range .NeedsHuman}}<tr><td>{{.JobID}}</td><td>{{.Flow}}</td><td>{{.Role}}</td><td>{{.Reason}}</td></tr>
{{else}}<tr><td colspan="4">— none —</td></tr>{{end}}</table>
</div>
</div>

<h2>Cost</h2>
<table><tr><th>job</th><th>role</th><th>state</th><th>tokens in/out</th><th>$ (micro-USD)</th><th>budget</th></tr>
{{range .Cost}}<tr><td>{{.JobID}}</td><td>{{.Role}}</td><td>{{.State}}</td>
<td>{{.TokensIn}} / {{.TokensOut}}</td><td>{{.MicroUSD}}</td>
<td>{{if .OverBudget}}<span class="over">over-budget</span>{{else}}ok{{end}}</td></tr>
{{end}}</table>

<h2>Audit (project-OUT, keyed job/action/head_sha)</h2>
<table><tr><th>job</th><th>action</th><th>head_sha</th><th>detail</th><th>at</th></tr>
{{range .Audit}}<tr><td>{{.JobID}}</td><td>{{.Action}}</td><td>{{.HeadSHA}}</td><td>{{.Detail}}</td><td>{{.ActedAt.Format "15:04:05"}}</td></tr>
{{else}}<tr><td colspan="5">— none —</td></tr>{{end}}</table>

<p>Live feed at <code>/v1/events</code>; JSON at <code>/v1/roster</code>,
<code>/v1/budget</code>, <code>/v1/cost</code>, <code>/v1/audit</code>,
<code>/v1/needs-human</code>.</p>
<script>
// live refresh: any lifecycle event on the SSE feed reloads the page so every
// pane (board/roster/budget/audit/cost) stays current (§12.6 "SSE").
try {
  const es = new EventSource("/v1/events");
  let t = null;
  es.onmessage = () => { clearTimeout(t); t = setTimeout(() => location.reload(), 300); };
} catch (e) {}
</script>
</body></html>`))

func renderDashboard(w http.ResponseWriter, d dashboardData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboardTmpl.Execute(w, d)
}
