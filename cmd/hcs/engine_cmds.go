package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/engine"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
)

// Códigos de salida de sync/plan.
const (
	exitOK        = 0
	exitAttention = 1 // conflicto, rechazo, secretos, bootstrap, snapshot frenado
	exitError     = 2
)

func newEngine(g globalFlags, stderr io.Writer) (*engine.Engine, bool) {
	if g.config == "" {
		_, _ = fmt.Fprintln(stderr, "hcs: falta --config")
		return nil, false
	}
	cfg, err := config.Load(g.config)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return nil, false
	}
	checker, err := makeChecker(g.checker)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return nil, false
	}
	e, err := engine.New(engine.Options{Config: cfg, StateDir: g.stateDir, AllowFileRemote: g.allowFileRemote, Checker: checker})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return nil, false
	}
	return e, true
}

func makeChecker(spec string) (ha.Checker, error) {
	switch {
	case spec == "" && os.Getenv("SUPERVISOR_TOKEN") != "":
		return nil, errors.New("el checker del Supervisor llega en M7; por ahora usá --checker noop o command:'…'")
	case spec == "", spec == "noop":
		return ha.Noop{}, nil
	case spec == "supervisor":
		return nil, errors.New("el checker del Supervisor llega en M7")
	case strings.HasPrefix(spec, "command:"):
		return ha.CommandChecker{Command: strings.TrimPrefix(spec, "command:")}, nil
	}
	return nil, fmt.Errorf("--checker %q no válido: noop | command:'<cmd>'", spec)
}

func cmdInit(g globalFlags, stdout, stderr io.Writer) int {
	e, ok := newEngine(g, stderr)
	if !ok {
		return exitError
	}
	res, err := e.Init(context.Background())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	}
	if res.Created {
		_, _ = fmt.Fprintln(stdout, "Se generó una deploy key nueva. Cargala en GitHub (Settings → Deploy keys → Allow write access):")
	} else {
		_, _ = fmt.Fprintln(stdout, "El state dir ya estaba inicializado. Deploy key:")
	}
	_, _ = fmt.Fprintln(stdout, res.DeployKey)
	return exitOK
}

func cmdDeployKey(g globalFlags, stdout, stderr io.Writer) int {
	pub, err := engine.DeployKey(g.stateDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs: no hay deploy key (¿corriste hcs init?):", err)
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
	_, _ = fmt.Fprintf(stdout, "estado:       %s\n", st.State)
	if !st.LastSync.IsZero() {
		_, _ = fmt.Fprintf(stdout, "último sync:  %s\n", st.LastSync.Format("2006-01-02 15:04:05"))
	}
	_, _ = fmt.Fprintf(stdout, "box:          %s\nmain:         %s\n", short(st.BoxHead), short(st.MainHead))
	if st.RestartRecommended {
		_, _ = fmt.Fprintln(stdout, "restart:      recomendado")
	}
	for _, p := range st.ConflictPaths {
		_, _ = fmt.Fprintf(stdout, "conflicto:    %s\n", p)
	}
	for _, f := range st.Findings {
		_, _ = fmt.Fprintf(stdout, "secreto:      %s\n", f)
	}
	if st.LastError != "" {
		_, _ = fmt.Fprintf(stdout, "error:        %s\n", st.LastError)
	}
	return exitOK
}

func cmdSync(g globalFlags, dryRun bool, stdout, stderr io.Writer) int {
	e, ok := newEngine(g, stderr)
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
		p("(dry-run: no se escribió nada)")
	}
	p("estado: %s (intentos: %d)", rep.State, rep.Attempts)
	if rep.Recovered != "" && rep.Recovered != "none" {
		p("recuperación de un apply interrumpido: %s", rep.Recovered)
	}
	if len(rep.Changes) > 0 {
		p("cambios del box (%s):", orDash(short(rep.SnapshotCommit)))
		for _, c := range rep.Changes {
			p("  %s %s", c.Kind, c.Path)
		}
	}
	for _, s := range rep.Skipped {
		p("  salteado: %s (%s)", s.RepoPath, s.Reason)
	}
	if rep.MergeKind != "" {
		note := ""
		if rep.DryRun && len(rep.Changes) > 0 {
			note = " (calculado sin los cambios del box de arriba, que todavía no se commitearon)"
		}
		p("merge: %s → %s%s", rep.MergeKind, orDash(short(rep.R)), note)
	}
	for _, c := range rep.ConflictPaths {
		p("  conflicto: %s", c)
	}
	for _, o := range rep.Overridden {
		p("  push_only conservado del box: %s", o)
	}
	if rep.Plan != nil && len(rep.Plan.Ops) > 0 {
		p("cambios hacia el box:")
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
			p("  no se aplica: %s (%s)", ig.RepoPath, ig.Reason)
		}
	}
	if rep.Applied {
		p("aplicado (check: %v, reload: %v)", rep.Checked, rep.Reloaded)
	}
	if rep.Restart {
		p("conviene reiniciar Home Assistant")
	}
	for _, s := range rep.Pushed {
		p("push: %s", s)
	}
	for _, f := range rep.Findings {
		p("  secreto: %s", f)
	}
	for _, f := range rep.Warnings {
		p("  advertencia: %s", f)
	}
	if rep.Error != "" {
		p("detalle: %s", rep.Error)
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
