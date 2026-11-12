package yamlcheck

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	tests := []struct {
		path    string
		content string
		wantErr string // empty = valid
	}{
		{"config/configuration.yaml", "homeassistant:\n  name: Home\n", ""},
		{"config/configuration.yaml", "", ""},
		{"config/configuration.yaml", "http:\n  api_password: !secret pw\nautomation: !include automations.yaml\n", ""},
		{"config/configuration.yaml", "sensor: !include_dir_merge_list sensors/\nx: !env_var HOME\n", ""},
		{"config/blueprints/a.yaml", "blueprint:\n  input:\n    light: !input light\n", ""},
		{"config/multi.yml", "a: 1\n---\nb: 2\n", ""},
		{"config/broken.yaml", "a: [1, 2\n", "yaml"},
		{"config/broken.yaml", "a:\n  b: 1\n c: 2\n", "line"},
		{"config/tabs.yaml", "a:\n\tb: 1\n", "yaml"},
		{"config/ok.json", `{"a": [1, 2]}`, ""},
		{"config/broken.json", "{\n\"a\": [1, 2\n}", "line 3"},
		{"config/script.py", "this is not yaml: [", ""},
		{"config/no_extension", "{{{", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path+"/"+tt.content, func(t *testing.T) {
			err := Check(tt.path, []byte(tt.content))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("want no error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("want an error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func FuzzCheck(f *testing.F) {
	for _, s := range []string{"a: 1\n", "x: !secret y\n", "{", "- [\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_ = Check("a.yaml", []byte(s)) // must not hang or panic
		_ = Check("a.json", []byte(s))
	})
}
