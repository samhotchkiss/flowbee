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
