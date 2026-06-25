package sessionrender

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/sessionstore"
)

func Transcript(w io.Writer, session sessionstore.Session, elements []sessionstore.Element) error {
	title := firstNonEmpty(session.Title, session.Preview, session.ID)
	fmt.Fprintf(w, "# %s - %s\n\n", session.ID, title)
	fmt.Fprintf(w, "Agent: %s\n", session.Agent)
	if session.ProjectID != "" {
		fmt.Fprintf(w, "Project: %s\n", session.ProjectID)
	}
	if session.UpdatedAt != "" {
		fmt.Fprintf(w, "Updated: %s\n", session.UpdatedAt)
	}
	if len(elements) == 0 {
		fmt.Fprintln(w, "\nNo timeline elements are available for this session yet.")
		return nil
	}
	for _, element := range elements {
		label := element.Type
		if element.Role != "" {
			label += " " + element.Role
		}
		fmt.Fprintf(w, "\n[%d] %s\n", element.Seq, label)
		if element.Model != "" || element.DurationMS > 0 || element.UsageJSON != "" {
			fmt.Fprint(w, "meta:")
			if element.Model != "" {
				fmt.Fprintf(w, " model=%s", element.Model)
			}
			if element.DurationMS > 0 {
				fmt.Fprintf(w, " duration=%s", time.Duration(element.DurationMS)*time.Millisecond)
			}
			if element.UsageJSON != "" {
				fmt.Fprintf(w, " usage=%s", element.UsageJSON)
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, element.ContentText)
	}
	return nil
}

func JSONL(w io.Writer, session sessionstore.Session, elements []sessionstore.Element) error {
	encoder := json.NewEncoder(w)
	if len(elements) == 0 {
		return encoder.Encode(map[string]any{
			"schemaVersion": "pax.session.v1",
			"type":          "session",
			"sessionId":     session.ID,
			"agent":         session.Agent,
			"nativeId":      session.NativeID,
			"title":         session.Title,
			"status":        session.Status,
			"updatedAt":     session.UpdatedAt,
		})
	}
	for _, element := range elements {
		record := map[string]any{
			"schemaVersion": "pax.session.v1",
			"sessionId":     session.ID,
			"seq":           element.Seq,
			"type":          element.Type,
		}
		set(record, "role", element.Role)
		set(record, "model", element.Model)
		set(record, "startedAt", element.StartedAt)
		set(record, "completedAt", element.CompletedAt)
		if element.DurationMS > 0 {
			record["durationMs"] = element.DurationMS
		}
		set(record, "contentText", element.ContentText)
		if element.UsageJSON != "" {
			var usage any
			if json.Unmarshal([]byte(element.UsageJSON), &usage) == nil {
				record["usage"] = usage
			}
		}
		if element.NormalizedRaw != nil {
			record["normalized"] = element.NormalizedRaw
		}
		if element.RawJSON != "" {
			var raw any
			if json.Unmarshal([]byte(element.RawJSON), &raw) == nil {
				record["raw"] = raw
			}
		}
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return nil
}

func HTML(w io.Writer, session sessionstore.Session, elements []sessionstore.Element) error {
	title := firstNonEmpty(session.Title, session.Preview, session.ID)
	counts := elementCounts(elements)
	stats := timelineStats(elements)
	fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>%s</title>", html.EscapeString(title))
	fmt.Fprint(w, `<style>
:root{--bg:#f4f2ec;--panel:#fffefa;--ink:#1f2933;--muted:#65727f;--line:#d9d4c8;--soft:#ebe6da;--blue:#2463a6;--green:#23705b;--amber:#8a5a12;--red:#a14343;--violet:#6652a3}
*{box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;background:var(--bg);color:var(--ink)}
main{max-width:1120px;margin:0 auto;padding:28px 18px 72px}
header{position:sticky;top:0;z-index:10;background:rgba(244,242,236,.94);backdrop-filter:blur(10px);border-bottom:1px solid var(--line);padding:18px 0 16px;margin-bottom:24px}
h1{font-size:24px;line-height:1.25;margin:0 0 12px;font-weight:720;letter-spacing:0}
.meta{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:8px 14px;color:var(--muted);font-size:13px}
.meta span{min-width:0;overflow-wrap:anywhere}
.summary{display:flex;flex-wrap:wrap;gap:8px;margin:16px 0 2px}
.chip{border:1px solid var(--line);background:var(--panel);border-radius:999px;padding:6px 10px;font-size:12px;color:var(--muted)}
.chip strong{color:var(--ink)}
.timeline{position:relative;margin-left:10px}
.timeline:before{content:"";position:absolute;left:19px;top:4px;bottom:4px;width:2px;background:var(--line)}
.item{position:relative;margin:14px 0 14px 46px;background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 2px rgba(31,41,51,.04)}
.item:before{content:"";position:absolute;left:-36px;top:18px;width:14px;height:14px;border-radius:50%;border:3px solid var(--bg);background:var(--muted)}
.item.message:before{background:var(--blue)}.item.thinking:before{background:var(--violet)}.item.tool_call:before{background:var(--amber)}.item.tool_result:before{background:var(--green)}.item.usage:before{background:var(--red)}.item.error:before{background:var(--red)}
.head{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;padding:12px 14px;border-bottom:1px solid var(--soft);font-size:13px;color:var(--muted)}
.left{display:flex;flex-wrap:wrap;align-items:center;gap:7px;min-width:0}.right{text-align:right;white-space:nowrap}
.time{display:block}.duration{display:block;color:var(--ink);font-weight:650}
.seq{font-variant-numeric:tabular-nums;color:var(--muted)}
.type{font-weight:720;color:var(--ink);text-transform:lowercase}
.role{border:1px solid var(--line);border-radius:999px;padding:2px 7px;color:var(--muted);background:#f7f5ef}
.content{padding:14px}
pre{white-space:pre-wrap;word-break:break-word;margin:0;font:13px/1.55 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
details{border-top:1px solid var(--soft);padding:10px 14px}
details summary{cursor:pointer;font-weight:650;color:var(--muted);font-size:13px}
details pre{margin-top:10px}
.toolmeta{display:grid;grid-template-columns:120px minmax(0,1fr);gap:8px 12px;margin-bottom:12px;font-size:13px}
.toolmeta dt{color:var(--muted);margin:0}.toolmeta dd{margin:0;min-width:0;overflow-wrap:anywhere;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.blocktitle{font-size:12px;font-weight:720;color:var(--muted);text-transform:uppercase;margin:14px 0 7px}
.jsonblock{background:#f7f5ef;border:1px solid var(--soft);border-radius:6px;padding:10px}
.usagegrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:8px;margin-bottom:12px}
.usagebox{background:#f7f5ef;border:1px solid var(--soft);border-radius:6px;padding:9px}.usagebox span{display:block;color:var(--muted);font-size:12px}.usagebox strong{font-size:16px}
.empty{background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:18px;color:var(--muted)}
@media (max-width:720px){main{padding:18px 12px 48px}.timeline{margin-left:0}.timeline:before{left:9px}.item{margin-left:28px}.item:before{left:-26px}.head{display:block}.right{text-align:left;margin-top:6px;white-space:normal}}
</style></head><body><main>`)
	fmt.Fprintf(w, "<header><h1>%s</h1><div class=\"meta\">", html.EscapeString(title))
	meta(w, "Session", session.ID)
	meta(w, "Agent", session.Agent)
	meta(w, "Updated", session.UpdatedAt)
	meta(w, "Project", session.ProjectID)
	if session.CurrentSyncVersion > 0 {
		meta(w, "Sync version", fmt.Sprint(session.CurrentSyncVersion))
	}
	meta(w, "Started", stats.Started)
	meta(w, "Ended", stats.Ended)
	if stats.Duration != "" {
		meta(w, "Overall duration", stats.Duration)
	}
	meta(w, "Exported", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprint(w, `</div><div class="summary">`)
	chip(w, "Elements", len(elements))
	chip(w, "Messages", counts["message"])
	chip(w, "Thinking", counts["thinking"])
	chip(w, "Tool calls", counts["tool_call"])
	chip(w, "Tool results", counts["tool_result"])
	if stats.TotalTokens > 0 {
		chip(w, "Total tokens", int(stats.TotalTokens))
	}
	fmt.Fprint(w, "</div></header>")
	if len(elements) == 0 {
		fmt.Fprint(w, `<div class="empty">No timeline elements are available for this session yet.</div>`)
		fmt.Fprint(w, "</main></body></html>")
		return nil
	}
	fmt.Fprint(w, `<div class="timeline">`)
	for _, element := range elements {
		long := len(element.ContentText) > 1200 || element.Type == "thinking" ||
			strings.HasPrefix(element.Type, "tool")
		fmt.Fprintf(w, `<section class="item %s">`, html.EscapeString(elementClass(element)))
		fmt.Fprint(w, `<div class="head"><div class="left">`)
		fmt.Fprintf(w, `<span class="seq">#%d</span><span class="type">%s</span>`, element.Seq, html.EscapeString(element.Type))
		if element.Role != "" {
			fmt.Fprintf(w, `<span class="role">%s</span>`, html.EscapeString(element.Role))
		}
		fmt.Fprint(w, `</div><div class="right">`)
		if element.Model != "" {
			fmt.Fprintf(w, `<span>%s</span> `, html.EscapeString(element.Model))
		}
		if element.DurationMS > 0 {
			fmt.Fprintf(w, `<span class="duration">%s</span>`, html.EscapeString((time.Duration(element.DurationMS) * time.Millisecond).String()))
		}
		if element.StartedAt != "" {
			fmt.Fprintf(w, `<span class="time">%s</span>`, html.EscapeString(shortStamp(element.StartedAt)))
		}
		fmt.Fprint(w, `</div></div><div class="content">`)
		if renderUsageElement(w, element) {
			fmt.Fprint(w, `</div></section>`)
			continue
		}
		if renderToolElement(w, element) {
			fmt.Fprint(w, `</div>`)
			if element.UsageJSON != "" {
				fmt.Fprintf(w, `<details><summary>Token usage</summary><pre>%s</pre></details>`, html.EscapeString(element.UsageJSON))
			}
			fmt.Fprint(w, `</section>`)
			continue
		}
		displayContent := cleanDisplayText(element.ContentText)
		content := html.EscapeString(displayContent)
		if long {
			preview := html.EscapeString(trimPreview(displayContent, 520))
			fmt.Fprintf(w, `<pre>%s</pre></div><details><summary>Expand full content</summary><pre>%s</pre></details>`, preview, content)
		} else {
			fmt.Fprintf(w, `<pre>%s</pre></div>`, content)
		}
		if element.UsageJSON != "" {
			fmt.Fprintf(w, `<details><summary>Token usage</summary><pre>%s</pre></details>`, html.EscapeString(element.UsageJSON))
		}
		fmt.Fprint(w, `</section>`)
	}
	fmt.Fprint(w, "</div>")
	fmt.Fprint(w, "</main></body></html>")
	return nil
}

type htmlStats struct {
	Started     string
	Ended       string
	Duration    string
	TotalTokens int64
}

func timelineStats(elements []sessionstore.Element) htmlStats {
	var stats htmlStats
	var first, last time.Time
	for _, element := range elements {
		started, ok := parseTime(element.StartedAt)
		if ok {
			if first.IsZero() || started.Before(first) {
				first = started
			}
			if started.After(last) {
				last = started
			}
		}
		completed, ok := parseTime(element.CompletedAt)
		if ok && completed.After(last) {
			last = completed
		}
		usage := usageStats(element.UsageJSON)
		if usage.Total.Total > 0 {
			stats.TotalTokens = usage.Total.Total
		}
	}
	if !first.IsZero() {
		stats.Started = first.Local().Format("Jan 02 15:04:05")
	}
	if !last.IsZero() {
		stats.Ended = last.Local().Format("Jan 02 15:04:05")
	}
	if !first.IsZero() && !last.IsZero() && last.After(first) {
		stats.Duration = last.Sub(first).Round(time.Millisecond).String()
	}
	return stats
}

func renderUsageElement(w io.Writer, element sessionstore.Element) bool {
	if element.Type != "usage" || element.UsageJSON == "" {
		return false
	}
	usage := usageStats(element.UsageJSON)
	fmt.Fprint(w, `<div class="usagegrid">`)
	usageBox(w, "Last total", usage.Last.Total)
	usageBox(w, "Last input", usage.Last.Input)
	usageBox(w, "Last cached", usage.Last.Cached)
	usageBox(w, "Last output", usage.Last.Output)
	usageBox(w, "Last reasoning", usage.Last.Reasoning)
	usageBox(w, "Session total", usage.Total.Total)
	usageBox(w, "Session input", usage.Total.Input)
	usageBox(w, "Session cached", usage.Total.Cached)
	usageBox(w, "Session output", usage.Total.Output)
	usageBox(w, "Session reasoning", usage.Total.Reasoning)
	fmt.Fprint(w, `</div>`)
	if element.UsageJSON != "" {
		renderToolValue(w, "Raw usage", mustPrettyJSON(element.UsageJSON))
	}
	return true
}

type tokenUsage struct {
	Total tokenUsageValues
	Last  tokenUsageValues
}

type tokenUsageValues struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
	Total     int64 `json:"total_tokens"`
}

func usageStats(raw string) tokenUsage {
	var decoded struct {
		Total tokenUsageValues `json:"total_token_usage"`
		Last  tokenUsageValues `json:"last_token_usage"`
	}
	_ = json.Unmarshal([]byte(raw), &decoded)
	return tokenUsage{Total: decoded.Total, Last: decoded.Last}
}

func usageBox(w io.Writer, label string, value int64) {
	fmt.Fprintf(w, `<div class="usagebox"><span>%s</span><strong>%d</strong></div>`, html.EscapeString(label), value)
}

func renderToolElement(w io.Writer, element sessionstore.Element) bool {
	if element.Type != "tool_call" && element.Type != "tool_result" {
		return false
	}
	if len(element.NormalizedRaw) == 0 {
		return false
	}
	fmt.Fprint(w, `<dl class="toolmeta">`)
	if name, ok := stringField(element.NormalizedRaw, "name"); ok {
		fmt.Fprintf(w, `<dt>Tool</dt><dd>%s</dd>`, html.EscapeString(name))
	}
	if callID, ok := stringField(element.NormalizedRaw, "callId"); ok {
		fmt.Fprintf(w, `<dt>Call ID</dt><dd>%s</dd>`, html.EscapeString(callID))
	}
	fmt.Fprint(w, `</dl>`)
	if element.Type == "tool_call" {
		renderToolValue(w, "Arguments", element.NormalizedRaw["arguments"])
	} else {
		renderToolValue(w, "Result", element.NormalizedRaw["output"])
	}
	return true
}

func renderToolValue(w io.Writer, label string, value any) {
	rendered := cleanDisplayText(prettyValue(value))
	if rendered == "" {
		rendered = "(empty)"
	}
	fmt.Fprintf(w, `<div class="blocktitle">%s</div>`, html.EscapeString(label))
	if len(rendered) > 1200 {
		preview := html.EscapeString(trimPreview(rendered, 520))
		fmt.Fprintf(w, `<pre class="jsonblock">%s</pre><details><summary>Expand full %s</summary><pre>%s</pre></details>`,
			preview, strings.ToLower(label), html.EscapeString(rendered))
		return
	}
	fmt.Fprintf(w, `<pre class="jsonblock">%s</pre>`, html.EscapeString(rendered))
}

func prettyValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		encoded, err := json.MarshalIndent(typed, "", "  ")
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}

func mustPrettyJSON(raw string) string {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return raw
	}
	encoded, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		return raw
	}
	return string(encoded)
}

func stringField(values map[string]any, key string) (string, bool) {
	value, ok := values[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok && text != ""
}

func meta(w io.Writer, label, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(w, "<span><strong>%s:</strong> %s</span>", html.EscapeString(label), html.EscapeString(value))
}

func chip(w io.Writer, label string, value int) {
	fmt.Fprintf(w, `<span class="chip">%s <strong>%d</strong></span>`, html.EscapeString(label), value)
}

func elementCounts(elements []sessionstore.Element) map[string]int {
	counts := map[string]int{}
	for _, element := range elements {
		counts[element.Type]++
	}
	return counts
}

func elementClass(element sessionstore.Element) string {
	if element.Type == "tool_call" || element.Type == "tool_result" || element.Type == "thinking" || element.Type == "message" || element.Type == "usage" {
		return element.Type
	}
	return "event"
}

func trimPreview(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n..."
}

func cleanDisplayText(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return r
		}
		if r < 0x20 || r == 0x7f {
			return '\uFFFD'
		}
		return r
	}, value)
}

func shortStamp(value string) string {
	parsed, ok := parseTime(value)
	if !ok {
		return value
	}
	return parsed.Local().Format("Jan 02 15:04:05")
}

func parseTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func set(record map[string]any, key, value string) {
	if value != "" {
		record[key] = value
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
