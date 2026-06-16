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
