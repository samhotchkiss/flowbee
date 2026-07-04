package api

import (
	"encoding/json"
	"html"
	"net/http"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/mailtrace"
)

func (s *Server) mailTrace(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeMailTrace(w, r) {
		return
	}
	if s.mailTraceDB == nil {
		http.Error(w, "mail trace database is not configured", http.StatusServiceUnavailable)
		return
	}
	messageID := strings.TrimSpace(r.PathValue("messageId"))
	if messageID == "" {
		http.Error(w, "missing message id", http.StatusBadRequest)
		return
	}
	trace, err := mailtrace.NewServiceWithDialect(s.mailTraceDB, s.mailTraceDialect).Trace(r.Context(), messageID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, "message not found", http.StatusNotFound)
			return
		}
		http.Error(w, "mail trace error", http.StatusInternalServerError)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		renderMailTraceHTML(w, trace)
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

func (s *Server) mailTraceMessages(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeMailTrace(w, r) {
		return
	}
	if s.mailTraceDB == nil {
		http.Error(w, "mail trace database is not configured", http.StatusServiceUnavailable)
		return
	}
	items, err := mailtrace.NewServiceWithDialect(s.mailTraceDB, s.mailTraceDialect).ListMessages(r.Context(), 100)
	if err != nil {
		http.Error(w, "mail trace message list error", http.StatusInternalServerError)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		writeJSON(w, http.StatusOK, map[string]any{"messages": items})
		return
	}
	renderMailTraceMessageListHTML(w, items)
}

func (s *Server) authorizeMailTrace(w http.ResponseWriter, r *http.Request) bool {
	if s.authn == nil {
		if !requestFromLoopback(r) {
			http.Error(w, "mail trace is available only from loopback unless worker auth is configured", http.StatusForbidden)
			return false
		}
		return true
	}
	if id, ok := auth.IdentityFrom(r); !ok || !isInternalMailTraceIdentity(id) {
		http.Error(w, "forbidden: mail trace requires an internal or superadmin identity", http.StatusForbidden)
		return false
	}
	return true
}

func isInternalMailTraceIdentity(id string) bool {
	return id == "superadmin" || id == "internal" ||
		strings.HasPrefix(id, "superadmin.") ||
		strings.HasPrefix(id, "internal.") ||
		strings.HasSuffix(id, ".superadmin") ||
		strings.HasSuffix(id, ".internal")
}

func renderMailTraceMessageListHTML(w http.ResponseWriter, items []mailtrace.MessageListItem) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>Mail Messages</title><style>`)
	b.WriteString(`body{font:14px system-ui,-apple-system,Segoe UI,sans-serif;margin:0;background:#0f1218;color:#e7eaf0}main{max-width:1180px;margin:0 auto;padding:28px}a{color:#93c5fd}table{border-collapse:collapse;width:100%}td,th{border-bottom:1px solid #28303c;padding:8px;text-align:left;vertical-align:top}.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}.muted{color:#9ca3af}`)
	b.WriteString(`</style></head><body><main><h1>Mail Messages</h1><table><thead><tr><th>Received</th><th>Subject</th><th>From</th><th>Status</th><th>Trace</th></tr></thead><tbody>`)
	for _, item := range items {
		received := ""
		if item.ReceivedAt != nil {
			received = *item.ReceivedAt
		}
		b.WriteString(`<tr><td class="muted">` + html.EscapeString(received) + `</td><td>` + html.EscapeString(item.Subject) + `<div class="mono muted">` + html.EscapeString(item.ID) + `</div></td><td>` + html.EscapeString(item.From) + `</td><td>` + html.EscapeString(item.ProcessingStatus) + `</td><td><a href="` + html.EscapeString(item.TraceURL) + `">Open trace</a></td></tr>`)
	}
	b.WriteString(`</tbody></table></main></body></html>`)
	_, _ = w.Write([]byte(b.String()))
}

func renderMailTraceHTML(w http.ResponseWriter, tr mailtrace.Trace) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>Mail Trace</title><style>`)
	b.WriteString(`body{font:14px system-ui,-apple-system,Segoe UI,sans-serif;margin:0;background:#0f1218;color:#e7eaf0}main{max-width:1180px;margin:0 auto;padding:28px}section{border-top:1px solid #303846;padding:20px 0}.kv{display:grid;grid-template-columns:220px 1fr;gap:6px 14px}.pill{display:inline-block;padding:2px 8px;border-radius:4px;background:#293241;font-size:12px}.complete{background:#14532d}.pending{background:#713f12}.missing{background:#4b5563}.skipped{background:#334155}.failed{background:#7f1d1d}pre{white-space:pre-wrap;background:#070a0f;border:1px solid #303846;border-radius:6px;padding:12px;overflow:auto}details{margin-top:10px}table{border-collapse:collapse;width:100%}td,th{border-bottom:1px solid #28303c;padding:7px;text-align:left;vertical-align:top}.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}`)
	b.WriteString(`</style></head><body><main>`)
	b.WriteString(`<h1>Mail Processing Trace</h1>`)
	b.WriteString(`<section><h2>Message</h2><div class="kv">`)
	row(&b, "Message ID", tr.MessageID)
	row(&b, "Subject", tr.Message.Subject)
	row(&b, "From", tr.Message.From)
	row(&b, "To", strings.Join(tr.Message.To, ", "))
	row(&b, "CC", strings.Join(tr.Message.CC, ", "))
	if tr.Message.ReceivedAt != nil {
		row(&b, "Received", *tr.Message.ReceivedAt)
	}
	row(&b, "Processing status", tr.Message.ProcessingStatus)
	b.WriteString(`</div></section>`)

	b.WriteString(`<section><h2>Deterministic Stage 1 <span class="pill ` + html.EscapeString(tr.Deterministic.Status) + `">` + html.EscapeString(tr.Deterministic.Status) + `</span></h2><div class="kv">`)
	if tr.Deterministic.Stage1Band != nil {
		row(&b, "Stage 1 band", *tr.Deterministic.Stage1Band)
	}
	if tr.Deterministic.Stage2PromptKey != nil {
		row(&b, "Stage 2 prompt key", *tr.Deterministic.Stage2PromptKey)
	}
	if tr.Deterministic.RankRationale != nil {
		row(&b, "Rank rationale", *tr.Deterministic.RankRationale)
	}
	b.WriteString(`</div><table><thead><tr><th>Fact</th><th>Value</th><th>Source</th><th>Raw</th></tr></thead><tbody>`)
	for _, k := range mailtrace.SortedFactKeys(tr.Deterministic.Facts) {
		f := tr.Deterministic.Facts[k]
		b.WriteString(`<tr><td class="mono">` + html.EscapeString(k) + `</td><td><pre>` + html.EscapeString(mustJSON(f.Value)) + `</pre></td><td class="mono">` + html.EscapeString(f.Source) + `</td><td><pre>` + html.EscapeString(mustJSON(f.Raw)) + `</pre></td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	renderJSONDetails(&b, "Routing decision", tr.Deterministic.RoutingDecision)
	renderJSONDetails(&b, "Raw details", tr.Deterministic.RawDetails)
	b.WriteString(`</section>`)

	renderLLM(&b, "Light LLM", tr.LightLLM.Status, tr.LightLLM.SkipReason, tr.LightLLM.Error, tr.LightLLM.Invocation, tr.LightLLM.RequestText, tr.LightLLM.ResponseText, tr.LightLLM.ParsedVerdict)
	heavyFields := map[string]any{"escalated": tr.HeavyLLM.Escalated, "escalation_reason": tr.HeavyLLM.EscalationReason, "context_bundle_manifest": tr.HeavyLLM.ContextBundleManifest, "parsed_output": tr.HeavyLLM.ParsedOutput}
	renderLLM(&b, "Heavy LLM", tr.HeavyLLM.Status, tr.HeavyLLM.SkipReason, tr.HeavyLLM.Error, tr.HeavyLLM.Invocation, tr.HeavyLLM.RequestText, tr.HeavyLLM.ResponseText, heavyFields)

	b.WriteString(`<section><h2>Raw Invocations</h2><table><thead><tr><th>ID</th><th>Stage</th><th>Model</th><th>Status</th><th>Request/Response</th></tr></thead><tbody>`)
	for _, inv := range tr.Invocations {
		stage := ""
		if inv.Invocation.Stage != nil {
			stage = *inv.Invocation.Stage
		}
		model := ""
		if inv.Invocation.Model != nil {
			model = *inv.Invocation.Model
		}
		b.WriteString(`<tr><td class="mono">` + html.EscapeString(inv.Invocation.ID) + `</td><td class="mono">` + html.EscapeString(stage) + `</td><td>` + html.EscapeString(model) + `</td><td>` + html.EscapeString(inv.Invocation.Status) + `</td><td>`)
		renderTextDetails(&b, "request", inv.RequestText)
		renderTextDetails(&b, "response", inv.ResponseText)
		b.WriteString(`</td></tr>`)
	}
	b.WriteString(`</tbody></table></section></main></body></html>`)
	_, _ = w.Write([]byte(b.String()))
}

func renderLLM(b *strings.Builder, title, status string, skipReason, errText *string, inv *mailtrace.InvocationMeta, requestText, responseText *string, parsed any) {
	b.WriteString(`<section><h2>` + html.EscapeString(title) + ` <span class="pill ` + html.EscapeString(status) + `">` + html.EscapeString(status) + `</span></h2>`)
	if skipReason != nil {
		row(b, "Skip reason", *skipReason)
	}
	if errText != nil {
		row(b, "Error", *errText)
	}
	renderJSONDetails(b, "Invocation metadata", inv)
	renderJSONDetails(b, "Parsed output", parsed)
	renderTextDetails(b, "Verbatim request", requestText)
	renderTextDetails(b, "Verbatim response", responseText)
	b.WriteString(`</section>`)
}

func row(b *strings.Builder, k, v string) {
	b.WriteString(`<div class="mono">` + html.EscapeString(k) + `</div><div>` + html.EscapeString(v) + `</div>`)
}

func renderJSONDetails(b *strings.Builder, title string, v any) {
	b.WriteString(`<details open><summary>` + html.EscapeString(title) + `</summary><pre>` + html.EscapeString(mustJSON(v)) + `</pre></details>`)
}

func renderTextDetails(b *strings.Builder, title string, v *string) {
	text := ""
	if v != nil {
		text = *v
	}
	b.WriteString(`<details><summary>` + html.EscapeString(title) + `</summary><pre>` + html.EscapeString(text) + `</pre></details>`)
}

func mustJSON(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
