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

// CommandChecker validates the config by running a shell command. If the
// output contains the JSON from Home Assistant's `check_config --json`, that
// result is used (errors and warnings); otherwise exit 0 is valid and any
// other exit is invalid with the output as errors. In v0 it is used to run
// the check_config of the HA image with Docker (scripts/ha-check.sh).
type CommandChecker struct {
	Command string
}

// CheckConfig implements Checker.
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

// parseCheckJSON parses the output of `check_config --json`:
//
//	{"total_errors": n, "total_warnings": m,
//	 "errors": {"domain": ["message", {config}…]}, "warnings": {…}}
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

// messages extracts the strings of each domain (the objects are the config
// HA attaches, not messages).
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
