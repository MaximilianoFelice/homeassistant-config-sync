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
	exitAttention = 1 // any state other than idle
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
		Config: cfg, StateDir: g.stateDir, FSRoot: g.fsRoot, AllowFileRemote: g.allowFileRemote,
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
	local := haClients{checker: noop, reloader: noop, notifier: noop}
	switch {
	case spec == "", spec == "noop":
		return local, nil
	case strings.HasPrefix(spec, "command:"):
		local.checker = ha.CommandChecker{Command: strings.TrimPrefix(spec, "command:")}
		return local, nil
	}
	return haClients{}, fmt.Errorf("invalid --checker %q: supervisor | noop | command:'<cmd>'", spec)
}

func cmdInit(g globalFlags, stdout, stderr io.Writer) int {
	e, _, ok := newEngine(g, stderr)
	if !ok {
		return exitError
	}
	if err := e.Init(context.Background()); err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	}
	_, _ = fmt.Fprintf(stdout, "State dir ready: %s\n", g.stateDir)
	return exitOK
}

func cmdStatus(g globalFlags, stdout, stderr io.Writer) int {
	st, err := engine.LoadState(g.stateDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	}
	if g.json {
		return printJSON(stdout, stderr, st)
	}
	_, _ = fmt.Fprintf(stdout, "state:        %s\n", st.State)
	if st.Review != "" {
		_, _ = fmt.Fprintf(stdout, "review:       %s\n", st.Review)
	}
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
	if st.CheckErrors != "" {
		_, _ = fmt.Fprintf(stdout, "check:        %s\n", st.CheckErrors)
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
	rep, err := e.Sync(context.Background(), engine.SyncOptions{DryRun: dryRun})
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
	if rep.Review != "" {
		p("review: %s", rep.Review)
	}
	if len(rep.Outgoing) > 0 {
		p("box changes (%s):", orDash(short(rep.SnapshotCommit)))
		for _, c := range rep.Outgoing {
			p("  %s %s", c.Status, c.Path)
		}
	}
	for _, c := range rep.ConflictPaths {
		p("  conflict: %s", c)
	}
	if len(rep.Incoming) > 0 {
		p("changes from main (%s):", orDash(short(rep.R)))
		for _, c := range rep.Incoming {
			p("  %s %s", c.Status, c.Path)
		}
	}
	for _, f := range rep.TooLarge {
		p("  too large: %s", f)
	}
	for _, c := range rep.Collisions {
		p("  collides with the app: %s", c)
	}
	if len(rep.Deleted) > 0 {
		p("  %d deletions from main held back", len(rep.Deleted))
	}
	if rep.Applied {
		p("applied (check: %v, reload: %v)", rep.Checked, rep.Reloaded)
	}
	if rep.CheckErrors != "" {
		p("config check errors: %s", rep.CheckErrors)
	}
	for _, w := range rep.CheckWarnings {
		p("  config check warning: %s", w)
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
