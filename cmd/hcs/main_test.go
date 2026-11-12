package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"no arguments", nil, 2, "", "Usage: hcs"},
		{"help", []string{"help"}, 0, "Usage: hcs", ""},
		{"--help", []string{"--help"}, 0, "Usage: hcs", ""},
		{"version", []string{"version"}, 0, "hcs dev", ""},
		{"unknown", []string{"bogus"}, 2, "", `unknown command "bogus"`},
		{"serve without config", []string{"serve"}, 2, "", "missing --config"},
		{"scan-secrets without paths", []string{"scan-secrets"}, 2, "", "usage: hcs scan-secrets"},
		{"sync without config", []string{"sync", "--state-dir", "/tmp/x", "--allow-file-remote"}, 2, "", "missing --config"},
		{"invalid checker", []string{"plan", "--config", "testdata/none.yaml", "--checker", "bogus"}, 2, "", "hcs:"},
		{"subcommand help", []string{"sync", "-h"}, 0, "", "-state-dir"},
		{"invalid flag", []string{"sync", "--nope"}, 2, "", "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestEveryCommandIsInUsage(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	for _, c := range commands {
		if !strings.Contains(buf.String(), c.name) {
			t.Errorf("usage does not mention %q", c.name)
		}
	}
}
