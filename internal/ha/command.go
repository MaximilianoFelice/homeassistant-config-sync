package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sort"
	"strings"
)

// CommandChecker valida la config corriendo un comando de shell. Si la
// salida trae el JSON de `check_config --json` de Home Assistant, se usa ese
// resultado (errores y warnings); si no, exit 0 es válida y cualquier otro
// exit es inválida con la salida como errores. Sirve en el v0 para correr el
// check_config de la imagen de HA con Docker (scripts/ha-check.sh).
type CommandChecker struct {
	Command string
}

// CheckConfig implementa Checker.
func (c CommandChecker) CheckConfig(ctx context.Context) (CheckResult, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", c.Command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return CheckResult{}, err
	}
	if res, ok := parseCheckJSON(stdout.Bytes()); ok {
		return res, nil
	}
	if err == nil {
		return CheckResult{Valid: true}, nil
	}
	out := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	return CheckResult{Valid: false, Errors: out}, nil
}

// parseCheckJSON interpreta la salida de `check_config --json`:
//
//	{"total_errors": n, "total_warnings": m,
//	 "errors": {"dominio": ["mensaje", {config}…]}, "warnings": {…}}
func parseCheckJSON(out []byte) (CheckResult, bool) {
	i := bytes.IndexByte(out, '{')
	if i < 0 {
		return CheckResult{}, false
	}
	var d struct {
		TotalErrors *int                         `json:"total_errors"`
		Errors      map[string][]json.RawMessage `json:"errors"`
		Warnings    map[string][]json.RawMessage `json:"warnings"`
	}
	if err := json.Unmarshal(out[i:], &d); err != nil || d.TotalErrors == nil {
		return CheckResult{}, false
	}
	errs := messages(d.Errors)
	res := CheckResult{Valid: *d.TotalErrors == 0, Warnings: messages(d.Warnings)}
	if !res.Valid {
		res.Errors = strings.Join(errs, "\n")
	}
	return res, true
}

// messages saca los strings de cada dominio (los objetos son la config que
// HA adjunta, no mensajes).
func messages(m map[string][]json.RawMessage) []string {
	domains := make([]string, 0, len(m))
	for d := range m {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	var out []string
	for _, d := range domains {
		for _, raw := range m[d] {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
