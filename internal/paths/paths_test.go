package paths

import (
	"path"
	"strings"
	"testing"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
)

func defaultMatcher(t testing.TB) *Matcher {
	t.Helper()
	ms := config.DefaultMappings()
	ms[1].Include = []string{"core_mosquitto/*.conf", "*_zigbee2mqtt/configuration.yaml"}
	m, err := New(ms)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestClassifyRepoPathWithDefaults(t *testing.T) {
	tests := []struct {
		repoPath string
		managed  bool
		dir      Direction
		reason   Reason
		boxPath  string
	}{
		// Lo que sí entra.
		{"config/configuration.yaml", true, Both, "", "/homeassistant/configuration.yaml"},
		{"config/automations.yaml", true, Both, "", "/homeassistant/automations.yaml"},
		{"config/packages/luces/cocina.yaml", true, Both, "", "/homeassistant/packages/luces/cocina.yaml"},
		{"config/blueprints/automation/x/y.yml", true, Both, "", "/homeassistant/blueprints/automation/x/y.yml"},
		{"config/esphome/living.yaml", true, Both, "", "/homeassistant/esphome/living.yaml"},
		{"config/python_scripts/hola.py", true, Both, "", "/homeassistant/python_scripts/hola.py"},
		{"config/pyscript/apps/a.py", true, Both, "", "/homeassistant/pyscript/apps/a.py"},
		{"config/custom_templates/macros.jinja", true, Both, "", "/homeassistant/custom_templates/macros.jinja"},
		{"config/.storage/lovelace", true, PushOnly, "", "/homeassistant/.storage/lovelace"},
		{"config/.storage/lovelace.dashboard_casa", true, PushOnly, "", "/homeassistant/.storage/lovelace.dashboard_casa"},
		{"config/.storage/lovelace_resources", true, PushOnly, "", "/homeassistant/.storage/lovelace_resources"},
		{"app_configs/core_mosquitto/mosquitto.conf", true, Both, "", "/addon_configs/core_mosquitto/mosquitto.conf"},
		{"app_configs/45df7312_zigbee2mqtt/configuration.yaml", true, Both, "", "/addon_configs/45df7312_zigbee2mqtt/configuration.yaml"},

		// Denylist dura: gana sobre la whitelist.
		{"config/secrets.yaml", false, 0, ReasonHardDenied, "/homeassistant/secrets.yaml"},
		{"config/esphome/secrets.yaml", false, 0, ReasonHardDenied, "/homeassistant/esphome/secrets.yaml"},
		{"config/Secrets.YAML", false, 0, ReasonHardDenied, "/homeassistant/Secrets.YAML"},
		{"config/home-assistant_v2.db", false, 0, ReasonHardDenied, "/homeassistant/home-assistant_v2.db"},
		{"config/home-assistant_v2.db-wal", false, 0, ReasonHardDenied, "/homeassistant/home-assistant_v2.db-wal"},
		{"config/home-assistant.log", false, 0, ReasonHardDenied, "/homeassistant/home-assistant.log"},
		{"config/home-assistant.log.1", false, 0, ReasonHardDenied, "/homeassistant/home-assistant.log.1"},
		{"config/.storage/auth", false, 0, ReasonHardDenied, "/homeassistant/.storage/auth"},
		{"config/.storage/auth_provider.homeassistant", false, 0, ReasonHardDenied, "/homeassistant/.storage/auth_provider.homeassistant"},
		{"config/.storage/core.config_entries", false, 0, ReasonHardDenied, "/homeassistant/.storage/core.config_entries"},
		{"config/.cloud/x.yaml", false, 0, ReasonHardDenied, "/homeassistant/.cloud/x.yaml"},
		{"config/.ssh/config.yaml", false, 0, ReasonHardDenied, "/homeassistant/.ssh/config.yaml"},
		{"config/certs/privkey.pem", false, 0, ReasonHardDenied, "/homeassistant/certs/privkey.pem"},

		// Exclusiones por defecto.
		{"config/custom_components/hacs/manifest.yaml", false, 0, ReasonExcluded, "/homeassistant/custom_components/hacs/manifest.yaml"},
		{"config/.cache/x.yaml", false, 0, ReasonExcluded, "/homeassistant/.cache/x.yaml"},
		{"config/deps/x.yaml", false, 0, ReasonExcluded, "/homeassistant/deps/x.yaml"},
		{"config/esphome/.esphome/build/x.yaml", false, 0, ReasonExcluded, "/homeassistant/esphome/.esphome/build/x.yaml"},
		{"config/ip_bans.yaml", false, 0, ReasonExcluded, "/homeassistant/ip_bans.yaml"},
		{"config/python_scripts/__pycache__/hola.py", false, 0, ReasonExcluded, "/homeassistant/python_scripts/__pycache__/hola.py"},

		// No incluidos.
		{"config/.storage/core.entity_registry", false, 0, ReasonNotIncluded, "/homeassistant/.storage/core.entity_registry"},
		{"config/go2rtc", false, 0, ReasonNotIncluded, "/homeassistant/go2rtc"},
		{"config/www/foto.jpg", false, 0, ReasonNotIncluded, "/homeassistant/www/foto.jpg"},
		{"config/.HA_VERSION", false, 0, ReasonNotIncluded, "/homeassistant/.HA_VERSION"},
		{"app_configs/a0d7b954_vscode/settings.json", false, 0, ReasonNotIncluded, "/addon_configs/a0d7b954_vscode/settings.json"},

		// Fuera de los mapeos: es del repo y nunca baja al box.
		{"README.md", false, 0, ReasonOutsideMappings, ""},
		{".github/workflows/ci.yaml", false, 0, ReasonOutsideMappings, ""},
		{"configuration.yaml", false, 0, ReasonOutsideMappings, ""},
		{"config", false, 0, ReasonOutsideMappings, ""},
		{"configx/a.yaml", false, 0, ReasonOutsideMappings, ""},

		// Paths que intentan escaparse.
		{"config/../etc/passwd", false, 0, ReasonInvalidPath, ""},
		{"config/./a.yaml", false, 0, ReasonInvalidPath, ""},
		{"/config/a.yaml", false, 0, ReasonInvalidPath, ""},
		{"config//a.yaml", false, 0, ReasonInvalidPath, ""},
		{"config/.git/hooks/post-checkout.yaml", false, 0, ReasonInvalidPath, ""},
		{"config/.GIT/x.yaml", false, 0, ReasonInvalidPath, ""},
		{"config/a\\..\\b.yaml", false, 0, ReasonInvalidPath, ""},
		{"config/a\x00.yaml", false, 0, ReasonInvalidPath, ""},
		{"", false, 0, ReasonInvalidPath, ""},
	}
	m := defaultMatcher(t)
	for _, tt := range tests {
		t.Run(tt.repoPath, func(t *testing.T) {
			r := m.ClassifyRepoPath(tt.repoPath)
			if r.Managed != tt.managed || r.Reason != tt.reason {
				t.Fatalf("managed=%v reason=%q, quería managed=%v reason=%q", r.Managed, r.Reason, tt.managed, tt.reason)
			}
			if r.Managed && r.Direction != tt.dir {
				t.Errorf("direction=%v, quería %v", r.Direction, tt.dir)
			}
			if r.BoxPath != tt.boxPath {
				t.Errorf("boxPath=%q, quería %q", r.BoxPath, tt.boxPath)
			}
		})
	}
}

func TestClassifyBoxPath(t *testing.T) {
	m := defaultMatcher(t)
	tests := []struct {
		boxPath  string
		managed  bool
		reason   Reason
		repoPath string
	}{
		{"/homeassistant/automations.yaml", true, "", "config/automations.yaml"},
		{"/homeassistant/packages/a.yaml", true, "", "config/packages/a.yaml"},
		{"/homeassistant/secrets.yaml", false, ReasonHardDenied, "config/secrets.yaml"},
		{"/addon_configs/core_mosquitto/mosquitto.conf", true, "", "app_configs/core_mosquitto/mosquitto.conf"},
		{"/homeassistant", false, ReasonOutsideMappings, ""},
		{"/homeassistant2/a.yaml", false, ReasonOutsideMappings, ""},
		{"/etc/passwd", false, ReasonOutsideMappings, ""},
		{"/homeassistant/../etc/passwd", false, ReasonInvalidPath, ""},
		{"homeassistant/a.yaml", false, ReasonInvalidPath, ""},
		{"/homeassistant/.git/config.yaml", false, ReasonInvalidPath, ""},
	}
	for _, tt := range tests {
		t.Run(tt.boxPath, func(t *testing.T) {
			r := m.ClassifyBoxPath(tt.boxPath)
			if r.Managed != tt.managed || r.Reason != tt.reason || r.RepoPath != tt.repoPath {
				t.Errorf("= {managed:%v reason:%q repo:%q}, quería {managed:%v reason:%q repo:%q}",
					r.Managed, r.Reason, r.RepoPath, tt.managed, tt.reason, tt.repoPath)
			}
		})
	}
}

func TestPushOnlyWinsOverInclude(t *testing.T) {
	m, err := New([]config.Mapping{{
		Repo: "config", Box: "/homeassistant",
		Include:  []string{"**/*.yaml"},
		PushOnly: []string{"lovelace/*.yaml"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if r := m.ClassifyRepoPath("config/lovelace/casa.yaml"); !r.Managed || r.Direction != PushOnly {
		t.Errorf("quería push-only, dio %+v", r)
	}
}

func TestExcludeWinsOverPushOnly(t *testing.T) {
	m, err := New([]config.Mapping{{
		Repo: "config", Box: "/homeassistant",
		PushOnly: []string{".storage/lovelace*"},
		Exclude:  []string{".storage/lovelace.privado"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if r := m.ClassifyRepoPath("config/.storage/lovelace.privado"); r.Managed || r.Reason != ReasonExcluded {
		t.Errorf("quería excluido, dio %+v", r)
	}
}

func TestHardDenyCannotBeOverridden(t *testing.T) {
	m, err := New([]config.Mapping{{
		Repo: "config", Box: "/homeassistant",
		Include:  []string{"**", "secrets.yaml", ".storage/**"},
		PushOnly: []string{"**"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"secrets.yaml", "home-assistant_v2.db", ".storage/auth", ".storage/core.config_entries", "x/id_ed25519", "home-assistant.log"} {
		if r := m.ClassifyRepoPath("config/" + p); r.Managed {
			t.Errorf("%s no debería gestionarse nunca: %+v", p, r)
		}
	}
}

func TestPruneDir(t *testing.T) {
	m := defaultMatcher(t)
	for dir, want := range map[string]bool{
		"custom_components":          true,
		"custom_components/hacs":     true,
		".cache":                     true,
		"deps":                       true,
		"esphome/.esphome":           true,
		".git":                       true,
		"sub/.git":                   true,
		".cloud":                     true,
		"x/.ssh":                     true,
		".storage":                   false, // adentro están los lovelace*
		"packages":                   false,
		"esphome":                    false,
		"python_scripts":             false,
		"python_scripts/__pycache__": true,
	} {
		if got := m.PruneDir(0, dir); got != want {
			t.Errorf("PruneDir(%q) = %v, quería %v", dir, got, want)
		}
	}
}

func TestNewRejectsInvalidGlob(t *testing.T) {
	if _, err := New([]config.Mapping{{Repo: "c", Box: "/c", Include: []string{"[x"}}}); err == nil {
		t.Error("quería error por glob inválido")
	}
}

func TestRepoAndBoxRoundTrip(t *testing.T) {
	m := defaultMatcher(t)
	for _, p := range []string{"config/a.yaml", "config/packages/x/y.yaml", "app_configs/core_mosquitto/m.conf"} {
		r := m.ClassifyRepoPath(p)
		back := m.ClassifyBoxPath(r.BoxPath)
		if back.RepoPath != p || back.Managed != r.Managed {
			t.Errorf("ida y vuelta de %q: %+v → %+v", p, r, back)
		}
	}
}

// FuzzClassifyRepoPath chequea la invariante de seguridad más importante: un
// path gestionado siempre cae dentro del directorio de su mapeo y nunca está
// en la denylist dura.
func FuzzClassifyRepoPath(f *testing.F) {
	for _, s := range []string{"config/a.yaml", "config/../a.yaml", "config/.storage/lovelace", "app_configs/x/y.yaml", "config/secrets.yaml", "config/a/../../b.yaml"} {
		f.Add(s)
	}
	m := defaultMatcher(f)
	f.Fuzz(func(t *testing.T, p string) {
		r := m.ClassifyRepoPath(p)
		if !r.Managed {
			return
		}
		mp := m.mappings[r.Mapping]
		if !strings.HasPrefix(r.BoxPath, mp.Box+"/") {
			t.Fatalf("%q gestionado fuera de su mapeo: %q", p, r.BoxPath)
		}
		if HardDenied(r.Rel) {
			t.Fatalf("%q gestionado pero en la denylist", p)
		}
		if path.Clean(p) != p || strings.Contains("/"+p+"/", "/../") || strings.Contains("/"+p+"/", "/.git/") {
			t.Fatalf("%q gestionado con un path sucio", p)
		}
	})
}
