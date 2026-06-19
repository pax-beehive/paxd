package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pax-beehive/paxd/internal/agentregistry"
	"github.com/pax-beehive/paxd/internal/sessionrender"
	"github.com/pax-beehive/paxd/internal/sessionstore"
	"github.com/urfave/cli/v3"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return newPaxCommand(stdout, stderr).Run(ctx, append([]string{"paxctl"}, args...))
}

func newPaxCommand(stdout, stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:  "paxctl",
		Usage: "Local-first agent session tools",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "db", Usage: "SQLite database path"},
		},
		Writer:    stdout,
		ErrWriter: stderr,
		Commands: []*cli.Command{
			{
				Name:  "agents",
				Usage: "Inspect local agent sources",
				Commands: []*cli.Command{
					{
						Name:  "list",
						Usage: "List supported local agents and adapters",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "format", Value: "table", Usage: "Output format: table or jsonl"},
							&cli.BoolFlag{Name: "probe", Usage: "Probe gateway-backed agents for live reachability"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return agentsList(cmd, stdout)
						},
					},
					{
						Name:  "setup",
						Usage: "Install local adapter commands for supported agents",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "agents", Usage: "Comma-separated agents to install"},
							&cli.BoolFlag{Name: "dry-run", Usage: "Print install commands without running them"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return agentsSetup(ctx, cmd, stdout, stderr)
						},
					},
				},
			},
			{
				Name:  "sessions",
				Usage: "List, sync, and render local agent sessions",
				Commands: []*cli.Command{
					{
						Name:  "list",
						Usage: "List session metadata",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "agents", Usage: "Comma-separated agents to scan"},
							&cli.StringFlag{Name: "updated-since", Usage: "Only show sessions updated since a duration like 24h or 7d"},
							&cli.IntFlag{Name: "limit", Usage: "Maximum sessions to show"},
							&cli.StringFlag{Name: "format", Value: "table", Usage: "Output format: table, jsonl, or html"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return sessionsList(ctx, cmd, stdout, stderr)
						},
					},
					{
						Name:  "sync",
						Usage: "Force sync session metadata and available timeline data",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "agents", Usage: "Comma-separated agents to sync"},
							&cli.StringFlag{Name: "updated-since", Usage: "Only sync sessions updated since a duration like 24h or 7d"},
							&cli.IntFlag{Name: "limit", Usage: "Maximum sessions to sync"},
							&cli.StringFlag{Name: "timeout", Value: "10s", Usage: "Per-agent ACP list timeout, for example 5s or 1m"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return sessionsSync(ctx, cmd, stdout, stderr)
						},
					},
					{
						Name:      "get",
						Usage:     "Render a session timeline",
						ArgsUsage: "<session-id>",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "agent", Usage: "Agent for bare native session IDs"},
							&cli.StringFlag{Name: "format", Value: "transcript", Usage: "Output format: transcript, jsonl, or html"},
							&cli.StringFlag{Name: "output", Usage: "Output path"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return sessionsGet(ctx, cmd, stdout)
						},
					},
				},
			},
		},
	}
}

func agentsList(cmd *cli.Command, stdout io.Writer) error {
	statuses, err := agentregistry.Default().StatusesWithProbe(nil, cmd.Bool("probe"))
	if err != nil {
		return err
	}
	switch cmd.String("format") {
	case "table":
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "AGENT\tSTATUS\tCAPABILITY\tCOMMAND\tSOURCE")
		for _, status := range statuses {
			state := firstNonEmpty(status.State, "missing")
			command := "-"
			if len(status.Command) > 0 {
				command = strings.Join(status.Command, " ")
			}
			capability := firstNonEmpty(status.Capability, "-")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", status.Agent.Name, state, capability, command, status.Agent.Source)
		}
		return tw.Flush()
	case "jsonl":
		encoder := json.NewEncoder(stdout)
		for _, status := range statuses {
			if err := encoder.Encode(map[string]any{
				"agent":      status.Agent.Name,
				"kind":       status.Agent.Kind,
				"available":  status.Available,
				"state":      firstNonEmpty(status.State, "missing"),
				"capability": status.Capability,
				"command":    status.Command,
				"source":     status.Agent.Source,
				"reason":     status.Reason,
			}); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported format %q", cmd.String("format"))
	}
}

func agentsSetup(ctx context.Context, cmd *cli.Command, stdout, stderr io.Writer) error {
	agents := parseCSV(cmd.String("agents"))
	explicit := len(agents) > 0
	statuses, err := agentregistry.Default().StatusesWithProbe(agents, false)
	if err != nil {
		return err
	}
	selected := 0
	for _, status := range statuses {
		if status.Available {
			if explicit {
				fmt.Fprintf(stdout, "%s already available via %s\n", status.Agent.Name, strings.Join(status.Command, " "))
			}
			continue
		}
		if len(status.Agent.InstallCommands) == 0 {
			if explicit {
				return fmt.Errorf("agent %s has no setup command: %s", status.Agent.Name, firstNonEmpty(status.Reason, status.Agent.InstallHint, "unsupported setup"))
			}
			continue
		}
		if !explicit && status.State != "installable" {
			continue
		}
		selected++
		if err := runAgentSetupCommands(ctx, stdout, stderr, status.Agent.Name, status.Agent.InstallCommands, cmd.Bool("dry-run")); err != nil {
			return err
		}
	}
	if selected == 0 && !explicit {
		fmt.Fprintln(stdout, "No installable agents found.")
	}
	return nil
}

func runAgentSetupCommands(ctx context.Context, stdout, stderr io.Writer, agent string, commands [][]string, dryRun bool) error {
	fmt.Fprintf(stdout, "Setting up %s\n", agent)
	for _, command := range commands {
		if len(command) == 0 {
			continue
		}
		fmt.Fprintf(stdout, "$ %s\n", strings.Join(command, " "))
		if dryRun {
			continue
		}
		proc := exec.CommandContext(ctx, command[0], command[1:]...)
		proc.Stdout = stderr
		proc.Stderr = stderr
		if err := proc.Run(); err != nil {
			return fmt.Errorf("setup %s: %s: %w", agent, strings.Join(command, " "), err)
		}
	}
	if !dryRun {
		fmt.Fprintf(stdout, "Setup complete for %s\n", agent)
	}
	return nil
}

func openSessionStore(cmd *cli.Command) (*sessionstore.Store, error) {
	return sessionstore.Open(cmd.String("db"))
}

func sessionsList(ctx context.Context, cmd *cli.Command, stdout, stderr io.Writer) error {
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	agents := parseCSV(cmd.String("agents"))
	limit := cmd.Int("limit")
	cutoff, err := parseUpdatedSince(cmd.String("updated-since"))
	if err != nil {
		return err
	}
	sessions, err := store.ListSessions(ctx, agents, limit)
	if err != nil {
		return err
	}
	sessions = filterSessions(sessions, cutoff, limit)
	return renderSessionList(stdout, sessions, cmd.String("format"))
}

func sessionsSync(ctx context.Context, cmd *cli.Command, stdout, stderr io.Writer) error {
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	agents := parseCSV(cmd.String("agents"))
	limit := cmd.Int("limit")
	cutoff, err := parseUpdatedSince(cmd.String("updated-since"))
	if err != nil {
		return err
	}
	timeout, err := parseDuration(cmd.String("timeout"))
	if err != nil {
		return fmt.Errorf("invalid timeout: %w", err)
	}
	if err := scanMetadata(ctx, store, agents, limit, timeout, stderr); err != nil {
		return err
	}
	sessions, err := store.ListSessions(ctx, agents, limit)
	if err != nil {
		return err
	}
	sessions = filterSessions(sessions, cutoff, limit)
	for _, session := range sessions {
		if err := syncSession(ctx, store, session); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "Synced %d session metadata records and available timelines.\n", len(sessions))
	return nil
}

func sessionsGet(ctx context.Context, cmd *cli.Command, stdout io.Writer) error {
	sessionID := cmd.Args().First()
	if sessionID == "" {
		return errors.New("usage: paxctl sessions get <session-id>")
	}
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	session, err := store.FindSession(ctx, sessionID, cmd.String("agent"))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session %q not found; run paxctl sessions list first", sessionID)
	}
	if err != nil {
		return err
	}
	if session.CurrentSyncVersion == 0 {
		if err := syncSession(ctx, store, session); err != nil {
			return err
		}
		session, err = store.FindSession(ctx, session.ID, "")
		if err != nil {
			return err
		}
	}
	elements, err := store.Elements(ctx, session)
	if err != nil {
		return err
	}

	format := cmd.String("format")
	output := cmd.String("output")
	var writer io.Writer = stdout
	var file *os.File
	if format == "html" {
		if output == "" {
			output = defaultHTMLPath(session)
		}
		file, err = os.Create(output)
		if err != nil {
			return err
		}
		defer file.Close()
		writer = file
	} else if output != "" {
		file, err = os.Create(output)
		if err != nil {
			return err
		}
		defer file.Close()
		writer = file
	}

	switch format {
	case "transcript":
		err = sessionrender.Transcript(writer, session, elements)
	case "jsonl":
		err = sessionrender.JSONL(writer, session, elements)
	case "html":
		err = sessionrender.HTML(writer, session, elements)
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
	if err != nil {
		return err
	}
	if format == "html" {
		fmt.Fprintf(stdout, "Wrote %s\n", output)
	}
	return nil
}

func scanMetadata(ctx context.Context, store *sessionstore.Store, agents []string, limit int, timeout time.Duration, stderr io.Writer) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	explicit := len(agents) > 0
	statuses, err := agentregistry.Default().Statuses(agents)
	if err != nil {
		return err
	}
	available := 0
	for _, status := range statuses {
		if !status.Available {
			if explicit {
				return fmt.Errorf("agent %s is unavailable: %s", status.Agent.Name, status.Reason)
			}
			continue
		}
		available++
		fmt.Fprintf(stderr, "syncing %s via %s (timeout %s)...\n", status.Agent.Name, strings.Join(status.Command, " "), timeout)
		sessions, err := agentregistry.ListSessions(ctx, status, timeout)
		if err != nil {
			if explicit {
				return fmt.Errorf("agent %s list sessions: %w", status.Agent.Name, err)
			}
			fmt.Fprintf(stderr, "warning: agent %s list failed: %v\n", status.Agent.Name, err)
			continue
		}
		if limit > 0 && len(sessions) > limit {
			sessions = sessions[:limit]
		}
		if err := store.UpsertSessions(ctx, status.Agent.Name, sessions); err != nil {
			return err
		}
	}
	if available == 0 {
		return errors.New("no supported local agents are available; run paxctl agents list")
	}
	return nil
}

func syncSession(ctx context.Context, store *sessionstore.Store, session sessionstore.Session) error {
	version, err := store.BeginSync(ctx, session.ID, session.Agent)
	if err != nil {
		return err
	}
	var elements []sessionstore.Element
	if session.Agent == "codex" {
		elements, err = agentregistry.CodexLocalElements(session.NativeID)
		if err != nil {
			_ = store.FailSync(ctx, version, err)
			return err
		}
	}
	if err := store.CompleteSync(ctx, session.ID, version, elements); err != nil {
		_ = store.FailSync(ctx, version, err)
		return err
	}
	return nil
}

func renderSessionList(w io.Writer, sessions []sessionstore.Session, format string) error {
	switch format {
	case "table":
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tAGENT\tUPDATED\tTITLE")
		for _, session := range sessions {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", session.ID, session.Agent, shortTime(session.UpdatedAt), firstNonEmpty(session.Title, session.Preview, "-"))
		}
		return tw.Flush()
	case "jsonl":
		encoder := json.NewEncoder(w)
		for _, session := range sessions {
			if err := encoder.Encode(map[string]any{
				"schemaVersion": "pax.session.metadata.v1",
				"id":            session.ID,
				"agent":         session.Agent,
				"nativeId":      session.NativeID,
				"title":         session.Title,
				"status":        session.Status,
				"preview":       session.Preview,
				"projectId":     session.ProjectID,
				"updatedAt":     session.UpdatedAt,
				"lastSyncedAt":  session.LastSyncedAt,
			}); err != nil {
				return err
			}
		}
		return nil
	case "html":
		return renderSessionListHTML(w, sessions)
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}

func renderSessionListHTML(w io.Writer, sessions []sessionstore.Session) error {
	if _, err := fmt.Fprintln(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>paxctl sessions</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:32px;color:#202124;background:#fafafa}
h1{font-size:24px;margin:0 0 20px}
table{width:100%;border-collapse:collapse;background:#fff;border:1px solid #ddd}
th,td{padding:10px 12px;border-bottom:1px solid #eee;text-align:left;vertical-align:top}
th{font-size:12px;text-transform:uppercase;color:#5f6368;background:#f5f5f5}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.empty{color:#5f6368}
</style>
</head>
<body>
<h1>paxctl sessions</h1>`); err != nil {
		return err
	}
	if len(sessions) == 0 {
		_, err := fmt.Fprintln(w, `<p class="empty">No local session metadata found. Run <code>paxctl sessions sync</code> to scan supported agents.</p></body></html>`)
		return err
	}
	if _, err := fmt.Fprintln(w, `<table><thead><tr><th>ID</th><th>Agent</th><th>Updated</th><th>Title</th></tr></thead><tbody>`); err != nil {
		return err
	}
	for _, session := range sessions {
		title := firstNonEmpty(session.Title, session.Preview, "-")
		if _, err := fmt.Fprintf(w, "<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			html.EscapeString(session.ID),
			html.EscapeString(session.Agent),
			html.EscapeString(shortTime(session.UpdatedAt)),
			html.EscapeString(title),
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, `</tbody></table></body></html>`)
	return err
}

func filterSessions(sessions []sessionstore.Session, cutoff *time.Time, limit int) []sessionstore.Session {
	out := make([]sessionstore.Session, 0, len(sessions))
	for _, session := range sessions {
		if cutoff != nil {
			updated, ok := parseSessionTime(firstNonEmpty(session.UpdatedAt, session.LastActive, session.LastListedAt))
			if ok && updated.Before(*cutoff) {
				continue
			}
		}
		out = append(out, session)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func parseCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseUpdatedSince(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	duration, err := parseDuration(value)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-duration)
	return &cutoff, nil
}

func parseDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(value)
}

func parseSessionTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func shortTime(value string) string {
	parsed, ok := parseSessionTime(value)
	if !ok {
		return value
	}
	return parsed.Local().Format("2006-01-02 15:04")
}

func defaultHTMLPath(session sessionstore.Session) string {
	safeID := strings.NewReplacer(":", "-", "/", "-", "\\", "-").Replace(session.ID)
	stamp := time.Now().Format("20060102-1504")
	return filepath.Join(".", "pax-session-"+safeID+"-"+stamp+".html")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
