package yamlcheck

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	tests := []struct {
		path    string
		content string
		wantErr string // vacío = válido
	}{
		{"config/configuration.yaml", "homeassistant:\n  name: Casa\n", ""},
		{"config/configuration.yaml", "", ""},
		{"config/configuration.yaml", "http:\n  api_password: !secret pw\nautomation: !include automations.yaml\n", ""},
		{"config/configuration.yaml", "sensor: !include_dir_merge_list sensors/\nx: !env_var HOME\n", ""},
		{"config/blueprints/a.yaml", "blueprint:\n  input:\n    luz: !input luz\n", ""},
		{"config/multi.yml", "a: 1\n---\nb: 2\n", ""},
		{"config/rota.yaml", "a: [1, 2\n", "yaml"},
		{"config/rota.yaml", "a:\n  b: 1\n c: 2\n", "line"},
		{"config/tabs.yaml", "a:\n\tb: 1\n", "yaml"},
		{"config/ok.json", `{"a": [1, 2]}`, ""},
		{"config/rota.json", "{\n\"a\": [1, 2\n}", "línea 3"},
		{"config/script.py", "esto no es yaml: [", ""},
		{"config/sin_extension", "{{{", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path+"/"+tt.content, func(t *testing.T) {
			err := Check(tt.path, []byte(tt.content))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("no quería error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("quería un error con %q, dio %v", tt.wantErr, err)
			}
		})
	}
}

func FuzzCheck(f *testing.F) {
	for _, s := range []string{"a: 1\n", "x: !secret y\n", "{", "- [\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_ = Check("a.yaml", []byte(s)) // no tiene que colgarse ni entrar en pánico
		_ = Check("a.json", []byte(s))
	})
}
