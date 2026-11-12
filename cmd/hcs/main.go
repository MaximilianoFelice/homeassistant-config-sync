// Command hcs is the homeassistant-config-sync engine: it syncs the Home
// Assistant configuration with a git repo in both directions.
// See docs/design.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

type command struct {
	name    string
	summary string
}

var commands = []command{
	{"init", "create the bare repo, the deploy key and known_hosts in the state dir"},
	{"status", "show the state of the last cycle"},
	{"plan", "show what a sync would do, without touching anything"},
	{"sync", "run one sync cycle"},
	{"serve", "start the HTTP API and the scheduler"},
	{"deploy-key", "show (or rotate) the deploy key's public key"},
	{"scan-secrets", "run the secret scanner over arbitrary paths"},
	{"version", "show the version"},
}

// globalFlags are the flags every subcommand accepts.
type globalFlags struct {
	config          string
	stateDir        string
	checker         string
	allowFileRemote bool
	json            bool

	// sync/plan only.
	dryRun          bool
	acceptBootstrap bool
	allowMassDelete bool

	// serve only.
	listen      string
	stdin       bool
	syncOnStart bool
}

func (g *globalFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.config, "config", "", "YAML/JSON file with the options.json schema")
	fs.StringVar(&g.stateDir, "state-dir", "/data", "engine state directory")
	fs.StringVar(&g.checker, "checker", "", "supervisor | noop | command:'<cmd>' (default: supervisor if SUPERVISOR_TOKEN is set, otherwise noop)")
	fs.BoolVar(&g.allowFileRemote, "allow-file-remote", false, "allow file:// remotes (v0 and tests only)")
	fs.BoolVar(&g.json, "json", false, "JSON output (status, sync, plan)")
	fs.BoolVar(&g.dryRun, "dry-run", false, "sync: compute without committing, applying or pushing")
	fs.BoolVar(&g.acceptBootstrap, "accept-bootstrap", false, "sync: confirm the first merge of box into an existing main")
	fs.BoolVar(&g.allowMassDelete, "allow-mass-delete", false, "sync: confirm a mass deletion")
	fs.StringVar(&g.listen, "listen", "127.0.0.1:8099", "serve: API address")
	fs.BoolVar(&g.stdin, "stdin", false, "serve: accept commands on stdin (\"sync\"), like hassio.addon_stdin")
	fs.BoolVar(&g.syncOnStart, "sync-on-start", false, "serve: run a sync on startup")
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	case "version", "--version":
		_, _ = fmt.Fprintf(stdout, "hcs %s\n", version)
		return 0
	}

	name := args[0]
	if !known(name) {
		_, _ = fmt.Fprintf(stderr, "hcs: unknown command %q\n\n", name)
		printUsage(stderr)
		return 2
	}

	fs := flag.NewFlagSet("hcs "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	g.register(fs)
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	switch name {
	case "scan-secrets":
		return scanSecrets(g, fs.Args(), stdout, stderr)
	case "init":
		return cmdInit(g, stdout, stderr)
	case "deploy-key":
		return cmdDeployKey(g, stdout, stderr)
	case "status":
		return cmdStatus(g, stdout, stderr)
	case "sync":
		return cmdSync(g, g.dryRun, stdout, stderr)
	case "plan":
		return cmdSync(g, true, stdout, stderr)
	case "serve":
		return cmdServe(g, stdout, stderr)
	}

	_, _ = fmt.Fprintf(stderr, "hcs %s: not implemented yet\n", name)
	return 1
}

func known(name string) bool {
	for _, c := range commands {
		if c.name == name {
			return true
		}
	}
	return false
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: hcs <command> [flags]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Commands:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range commands {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Run 'hcs <command> -h' to see its flags.")
}
