package scan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

const maxSize = 1024

// setup crea un box temporal con los mapeos por defecto apuntando ahí.
func setup(t *testing.T) (ha string, m *paths.Matcher) {
	t.Helper()
	root := t.TempDir()
	ms := config.DefaultMappings()
	ms[0].Box = filepath.Join(root, "homeassistant")
	ms[1].Box = filepath.Join(root, "addon_configs")
	if err := os.MkdirAll(ms[0].Box, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := paths.New(ms)
	if err != nil {
		t.Fatal(err)
	}
	return ms[0].Box, m
}

func run(t *testing.T, m *paths.Matcher, opts Options) *Result {
	t.Helper()
	if opts.MaxFileSize == 0 {
		opts.MaxFileSize = maxSize
	}
	res, err := Scan(m, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestScanClassifiesTheBox(t *testing.T) {
	ha, m := setup(t)
	w := func(p, c string) { testkit.WriteFile(t, filepath.Join(ha, p), c) }
	w("configuration.yaml", "homeassistant:\n")
	w("automations.yaml", "[]\n")
	w("packages/luces.yaml", "light:\n")
	w(".storage/lovelace", "{}\n")
	w("secrets.yaml", "pw: 123\n")
	w("home-assistant_v2.db", "SQLite")
	w("custom_components/hacs/services.yaml", "x:\n")
	w(".storage/core.entity_registry", "{}\n")
	w("go2rtc", "ELF")
	w("grande.yaml", strings.Repeat("x", maxSize+1))
	w("binario.yaml", "abc\x00def")
	testkit.WriteFileMode(t, filepath.Join(ha, "python_scripts/hola.py"), "print(1)\n", 0o755)
	if err := os.Symlink(filepath.Join(ha, "automations.yaml"), filepath.Join(ha, "link.yaml")); err != nil {
		t.Fatal(err)
	}

	res := run(t, m, Options{})

	want := map[string]string{
		"config/configuration.yaml":     ModeFile,
		"config/automations.yaml":       ModeFile,
		"config/packages/luces.yaml":    ModeFile,
		"config/.storage/lovelace":      ModeFile,
		"config/python_scripts/hola.py": ModeExec,
	}
	if len(res.Files) != len(want) {
		t.Errorf("archivos = %v, quería %v", keys(res.Files), want)
	}
	for p, mode := range want {
		f, ok := res.Files[p]
		if !ok {
			t.Errorf("falta %s", p)
			continue
		}
		if f.Mode != mode {
			t.Errorf("%s: modo %s, quería %s", p, f.Mode, mode)
		}
		if f.OID != BlobOID(f.Content) {
			t.Errorf("%s: OID no coincide con el contenido", p)
		}
	}
	if d := res.Files["config/.storage/lovelace"].Direction; d != paths.PushOnly {
		t.Errorf("lovelace: dirección %v, quería push", d)
	}

	gotSkips := map[string]SkipReason{}
	for _, s := range res.Skipped {
		gotSkips[s.RepoPath] = s.Reason
	}
	wantSkips := map[string]SkipReason{
		"config/grande.yaml":  SkipTooLarge,
		"config/binario.yaml": SkipBinary,
		"config/link.yaml":    SkipSymlink,
	}
	if len(gotSkips) != len(wantSkips) {
		t.Errorf("salteados = %v, quería %v", gotSkips, wantSkips)
	}
	for p, r := range wantSkips {
		if gotSkips[p] != r {
			t.Errorf("%s: motivo %q, quería %q", p, gotSkips[p], r)
		}
	}

	if len(res.Roots) != 2 || !res.Roots[0].Active || !res.Roots[0].Exists || res.Roots[1].Active {
		t.Errorf("raíces = %+v", res.Roots)
	}
	if res.Roots[0].Managed != 7 { // 5 archivos + grande + binario (el symlink no cuenta)
		t.Errorf("managed = %d, quería 7", res.Roots[0].Managed)
	}
}

func TestScanMissingRoot(t *testing.T) {
	ha, m := setup(t)
	testkit.Remove(t, ha)
	res := run(t, m, Options{})
	if res.Roots[0].Exists || !res.Roots[0].Active {
		t.Errorf("raíz = %+v, quería activa e inexistente", res.Roots[0])
	}
}

// old pone el archivo en el pasado, fuera de la ventana "racy".
func old(t *testing.T, p string) time.Time {
	t.Helper()
	ts := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestScanCacheSkipsUnchangedFiles(t *testing.T) {
	ha, m := setup(t)
	p := filepath.Join(ha, "automations.yaml")
	testkit.WriteFile(t, p, "aaaa\n")
	ts := old(t, p)

	first := run(t, m, Options{})
	if _, ok := first.Cache["config/automations.yaml"]; !ok {
		t.Fatal("un archivo viejo debería quedar en la cache")
	}

	second := run(t, m, Options{Cache: first.Cache})
	if f := second.Files["config/automations.yaml"]; f.Content != nil || f.OID != first.Files["config/automations.yaml"].OID {
		t.Errorf("segundo escaneo: quería un hit de cache con el mismo OID, dio %+v", f)
	}

	// Mismo tamaño y mismo mtime restaurado a mano: el ctime cambió, así que
	// la cache no puede esconder la edición.
	testkit.WriteFile(t, p, "bbbb\n")
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
	third := run(t, m, Options{Cache: second.Cache})
	f := third.Files["config/automations.yaml"]
	if f.Content == nil || string(f.Content) != "bbbb\n" || f.OID != BlobOID([]byte("bbbb\n")) {
		t.Errorf("tercer escaneo: la edición quedó escondida por la cache: %+v", f)
	}
}

func TestScanDoesNotCacheRacyFiles(t *testing.T) {
	ha, m := setup(t)
	testkit.WriteFile(t, filepath.Join(ha, "automations.yaml"), "x\n")
	res := run(t, m, Options{})
	if _, ok := res.Cache["config/automations.yaml"]; ok {
		t.Error("un archivo recién escrito no debería entrar en la cache")
	}
	if _, ok := res.Files["config/automations.yaml"]; !ok {
		t.Error("pero sí en el resultado")
	}
}

func TestScanRetriesFilesThatChangeWhileRead(t *testing.T) {
	ha, m := setup(t)
	p := filepath.Join(ha, "automations.yaml")
	testkit.WriteFile(t, p, "v1\n")

	// Cambia solo durante el primer intento: el segundo lee estable.
	res := run(t, m, Options{afterRead: func(bp string, attempt int) {
		if bp == p && attempt == 0 {
			testkit.WriteFile(t, p, "version 2\n")
		}
	}})
	if got := string(res.Files["config/automations.yaml"].Content); got != "version 2\n" {
		t.Errorf("contenido = %q, quería el estable", got)
	}

	// Cambia en todos los intentos: se saltea como inestable.
	n := 0
	res = run(t, m, Options{afterRead: func(bp string, _ int) {
		if bp == p {
			n++
			testkit.WriteFile(t, p, strings.Repeat("z", n)+"\n")
		}
	}})
	if _, ok := res.Files["config/automations.yaml"]; ok {
		t.Error("un archivo inestable no debería entrar")
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != SkipUnstable {
		t.Errorf("salteados = %+v, quería unstable", res.Skipped)
	}
}

func TestScanUnreadableDirIsSkippedNotDeleted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("como root los permisos no bloquean la lectura")
	}
	ha, m := setup(t)
	testkit.WriteFile(t, filepath.Join(ha, "packages/a.yaml"), "a:\n")
	testkit.WriteFile(t, filepath.Join(ha, "configuration.yaml"), "x:\n")
	dir := filepath.Join(ha, "packages")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	res := run(t, m, Options{})
	if len(res.Skipped) != 1 || !res.Skipped[0].Dir || res.Skipped[0].Reason != SkipUnreadable {
		t.Fatalf("salteados = %+v", res.Skipped)
	}
	if !res.Skipped[0].Covers("config/packages/a.yaml") || res.Skipped[0].Covers("config/packagesx/a.yaml") {
		t.Error("Covers no respeta el prefijo de directorio")
	}
}

func TestReadVerified(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.yaml")
	testkit.WriteFile(t, p, "a\n")
	if _, err := ReadVerified(p, BlobOID([]byte("a\n")), maxSize); err != nil {
		t.Errorf("mismo contenido: %v", err)
	}
	if _, err := ReadVerified(p, BlobOID([]byte("b\n")), maxSize); !IsUnstable(err) {
		t.Errorf("contenido distinto: quería unstable, dio %v", err)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
