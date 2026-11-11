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
		{"sin argumentos", nil, 2, "", "Uso: hcs"},
		{"help", []string{"help"}, 0, "Uso: hcs", ""},
		{"--help", []string{"--help"}, 0, "Uso: hcs", ""},
		{"version", []string{"version"}, 0, "hcs dev", ""},
		{"desconocido", []string{"bogus"}, 2, "", `comando desconocido "bogus"`},
		{"serve sin config", []string{"serve"}, 2, "", "falta --config"},
		{"scan-secrets sin paths", []string{"scan-secrets"}, 2, "", "uso: hcs scan-secrets"},
		{"sync sin config", []string{"sync", "--state-dir", "/tmp/x", "--allow-file-remote"}, 2, "", "falta --config"},
		{"checker inválido", []string{"plan", "--config", "testdata/none.yaml", "--checker", "bogus"}, 2, "", "hcs:"},
		{"help de subcomando", []string{"sync", "-h"}, 0, "", "-state-dir"},
		{"flag inválido", []string{"sync", "--nope"}, 2, "", "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("código = %d, quería %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, quería que contenga %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, quería que contenga %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestEveryCommandIsInUsage(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	for _, c := range commands {
		if !strings.Contains(buf.String(), c.name) {
			t.Errorf("el uso no menciona %q", c.name)
		}
	}
}
