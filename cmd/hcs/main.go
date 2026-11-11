// Command hcs es el motor de homeassistant-config-sync: sincroniza en las
// dos direcciones la configuración de Home Assistant con un repo git.
// Ver docs/design.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
)

// version se pisa en build con -ldflags "-X main.version=...".
var version = "dev"

type command struct {
	name    string
	summary string
}

var commands = []command{
	{"init", "crea el repo bare, la deploy key y known_hosts en el state dir"},
	{"status", "muestra el estado del último ciclo"},
	{"plan", "muestra lo que haría un sync, sin tocar nada"},
	{"sync", "corre un ciclo de sync"},
	{"serve", "levanta la API HTTP y el scheduler"},
	{"deploy-key", "muestra (o rota) la clave pública de la deploy key"},
	{"scan-secrets", "corre el escáner de secretos sobre paths sueltos"},
	{"version", "muestra la versión"},
}

// globalFlags son los flags que aceptan todos los subcomandos.
type globalFlags struct {
	config          string
	stateDir        string
	checker         string
	allowFileRemote bool
}

func (g *globalFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.config, "config", "", "archivo YAML/JSON con el esquema de options.json")
	fs.StringVar(&g.stateDir, "state-dir", "/data", "directorio de estado del motor")
	fs.StringVar(&g.checker, "checker", "", "supervisor | noop | command:'<cmd>' (default: supervisor si hay SUPERVISOR_TOKEN, si no noop)")
	fs.BoolVar(&g.allowFileRemote, "allow-file-remote", false, "permite remotos file:// (solo v0 y tests)")
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
		_, _ = fmt.Fprintf(stderr, "hcs: comando desconocido %q\n\n", name)
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

	if name == "scan-secrets" {
		return scanSecrets(g, fs.Args(), stdout, stderr)
	}

	_, _ = fmt.Fprintf(stderr, "hcs %s: todavía no implementado\n", name)
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
	_, _ = fmt.Fprintln(w, "Uso: hcs <comando> [flags]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Comandos:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range commands {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Usá 'hcs <comando> -h' para ver los flags.")
}
