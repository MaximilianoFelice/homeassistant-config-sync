package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/engine"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
)

// Exit codes of sync/plan.
const (
	exitOK        = 0
	exitAttention = 1 // conflict, rejection, secrets, bootstrap, stopped snapshot
	exitError     = 2
)

func newEngine(g globalFlags, stderr io.Writer) (*engine.Engine, *config.Config, bool) {
	if g.config == "" {
		_, _ = fmt.Fprintln(stderr, "hcs: missing --config")
		return nil, nil, false
	}
	cfg, err := config.Load(g.config)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return nil, nil, false
	}
	hac, err := makeHA(g.checker, cfg.NotifyService)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return nil, nil, false
	}
	e, err := engine.New(engine.Options{
		Config: cfg, StateDir: g.stateDir, AllowFileRemote: g.allowFileRemote,
		Checker: hac.checker, Reloader: hac.reloader, Notifier: hac.notifier,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return nil, nil, false
	}
	return e, cfg, true
}

// haClients groups what the engine needs from Home Assistant.
type haClients struct {
	checker  ha.Checker
	reloader ha.Reloader
	notifier ha.Notifier
}

// makeHA picks the HA clients. Inside the app (SUPERVISOR_TOKEN is set)
// everything goes through the Supervisor; locally, the checker comes from
// --checker and reload and notifications are no-ops.
func makeHA(spec, notifyService string) (haClients, error) {
	inApp := os.Getenv("SUPERVISOR_TOKEN") != ""
	if spec == "supervisor" || (spec == "" && inApp) {
		s, err := ha.NewSupervisorFromEnv(notifyService)
		if err != nil {
			return haClients{}, err
		}
		return haClients{checker: s, reloader: s, notifier: s}, nil
	}
	noop := ha.Noop{}
	switch {
	case spec == "", spec == "noop":
		return haClients{checker: noop, reloader: noop, notifier: noop}, nil
	case strings.HasPrefix(spec, "command:"):
		return haClients{checker: ha.CommandChecker{Command: strings.TrimPrefix(spec, "command:")}, reloader: noop, notifier: noop}, nil
	}
	return haClients{}, fmt.Errorf("invalid --checker %q: supervisor | noop | command:'<cmd>'", spec)
}

func cmdInit(g globalFlags, stdout, stderr io.Writer) int {
	e, _, ok := newEngine(g, stderr)
	if !ok {
		return exitError
	}
	res, err := e.Init(context.Background())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	}
	if res.Created {
		_, _ = fmt.Fprintln(stdout, "Generated a new deploy key. Add it on GitHub (Settings → Deploy keys → Allow write access):")
	} else {
		_, _ = fmt.Fprintln(stdout, "The state dir was already initialized. Deploy key:")
	}
	_, _ = fmt.Fprintln(stdout, res.DeployKey)
	return exitOK
}

func cmdDeployKey(g globalFlags, stdout, stderr io.Writer) int {
	pub, err := engine.DeployKey(g.stateDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs: no deploy key (did you run hcs init?):", err)
		return exitError
	}
	_, _ = fmt.Fprintln(stdout, pub)
	return exitOK
}

func cmdStatus(g globalFlags, stdout, stderr io.Writer) int {
	st, err := engine.LoadState(g.stateDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	}
	if g.json {
		st.Cache = nil
		return printJSON(stdout, stderr, st)
	}
	_, _ = fmt.Fprintf(stdout, "state:        %s\n", st.State)
	if !st.LastSync.IsZero() {
		_, _ = fmt.Fprintf(stdout, "last sync:    %s\n", st.LastSync.Format("2006-01-02 15:04:05"))
	}
	_, _ = fmt.Fprintf(stdout, "box:          %s\nmain:         %s\n", short(st.BoxHead), short(st.MainHead))
	if st.RestartRecommended {
		_, _ = fmt.Fprintln(stdout, "restart:      recommended")
	}
	for _, p := range st.ConflictPaths {
		_, _ = fmt.Fprintf(stdout, "conflict:     %s\n", p)
	}
	for _, f := range st.Findings {
		_, _ = fmt.Fprintf(stdout, "secret:       %s\n", f)
	}
	if st.LastError != "" {
		_, _ = fmt.Fprintf(stdout, "error:        %s\n", st.LastError)
	}
	return exitOK
}

func cmdSync(g globalFlags, dryRun bool, stdout, stderr io.Writer) int {
	e, _, ok := newEngine(g, stderr)
	if !ok {
		return exitError
	}
	rep, err := e.Sync(context.Background(), engine.SyncOptions{
		DryRun: dryRun, AcceptBootstrap: g.acceptBootstrap, AllowMassDelete: g.allowMassDelete,
	})
	if rep == nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	}
	if g.json {
		if code := printJSON(stdout, stderr, rep); code != exitOK {
			return code
		}
	} else {
		printReport(stdout, rep)
	}
	switch {
	case err != nil:
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	case rep.State == engine.StateIdle:
		return exitOK
	default:
		return exitAttention
	}
}

func printReport(w io.Writer, rep *engine.Report) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	if rep.DryRun {
		p("(dry-run: nothing was written)")
	}
	p("state: %s (attempts: %d)", rep.State, rep.Attempts)
	if rep.Recovered != "" && rep.Recovered != "none" {
		p("recovered an interrupted apply: %s", rep.Recovered)
	}
	if len(rep.Changes) > 0 {
		p("box changes (%s):", orDash(short(rep.SnapshotCommit)))
		for _, c := range rep.Changes {
			p("  %s %s", c.Kind, c.Path)
		}
	}
	for _, s := range rep.Skipped {
		p("  skipped: %s (%s)", s.RepoPath, s.Reason)
	}
	if rep.MergeKind != "" {
		note := ""
		if rep.DryRun && len(rep.Changes) > 0 {
			note = " (computed without the uncommitted box changes above)"
		}
		p("merge: %s → %s%s", rep.MergeKind, orDash(short(rep.R)), note)
	}
	for _, c := range rep.ConflictPaths {
		p("  conflict: %s", c)
	}
	for _, o := range rep.Overridden {
		p("  push_only kept from the box: %s", o)
	}
	if rep.Plan != nil && len(rep.Plan.Ops) > 0 {
		p("changes to the box:")
		for _, op := range rep.Plan.Ops {
			kind := "W"
			if op.IsDelete() {
				kind = "D"
			}
			p("  %s %s", kind, op.RepoPath)
		}
	}
	if rep.Plan != nil {
		for _, ig := range rep.Plan.Ignored {
			p("  not applied: %s (%s)", ig.RepoPath, ig.Reason)
		}
	}
	if rep.Applied {
		p("applied (check: %v, reload: %v)", rep.Checked, rep.Reloaded)
	}
	if rep.Restart {
		p("a Home Assistant restart is recommended")
	}
	for _, s := range rep.Pushed {
		p("push: %s", s)
	}
	for _, f := range rep.Findings {
		p("  secret: %s", f)
	}
	for _, f := range rep.Warnings {
		p("  warning: %s", f)
	}
	if rep.Error != "" {
		p("detail: %s", rep.Error)
	}
}

func printJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	}
	return exitOK
}

func short(oid string) string {
	if len(oid) > 10 {
		return oid[:10]
	}
	return oid
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
