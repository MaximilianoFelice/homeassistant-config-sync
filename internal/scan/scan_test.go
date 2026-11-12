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

// setup creates a temporary box with the default mappings pointing at it.
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
	w("packages/lights.yaml", "light:\n")
	w(".storage/lovelace", "{}\n")
	w("secrets.yaml", "pw: 123\n")
	w("home-assistant_v2.db", "SQLite")
	w("custom_components/hacs/services.yaml", "x:\n")
	w(".storage/core.entity_registry", "{}\n")
	w("go2rtc", "ELF")
	w("large.yaml", strings.Repeat("x", maxSize+1))
	w("binary.yaml", "abc\x00def")
	testkit.WriteFileMode(t, filepath.Join(ha, "python_scripts/hello.py"), "print(1)\n", 0o755)
	if err := os.Symlink(filepath.Join(ha, "automations.yaml"), filepath.Join(ha, "link.yaml")); err != nil {
		t.Fatal(err)
	}

	res := run(t, m, Options{})

	want := map[string]string{
		"config/configuration.yaml":      ModeFile,
		"config/automations.yaml":        ModeFile,
		"config/packages/lights.yaml":    ModeFile,
		"config/.storage/lovelace":       ModeFile,
		"config/python_scripts/hello.py": ModeExec,
	}
	if len(res.Files) != len(want) {
		t.Errorf("files = %v, want %v", keys(res.Files), want)
	}
	for p, mode := range want {
		f, ok := res.Files[p]
		if !ok {
			t.Errorf("missing %s", p)
			continue
		}
		if f.Mode != mode {
			t.Errorf("%s: mode %s, want %s", p, f.Mode, mode)
		}
		if f.OID != BlobOID(f.Content) {
			t.Errorf("%s: OID does not match the content", p)
		}
	}
	if d := res.Files["config/.storage/lovelace"].Direction; d != paths.PushOnly {
		t.Errorf("lovelace: direction %v, want push", d)
	}

	gotSkips := map[string]SkipReason{}
	for _, s := range res.Skipped {
		gotSkips[s.RepoPath] = s.Reason
	}
	wantSkips := map[string]SkipReason{
		"config/large.yaml":  SkipTooLarge,
		"config/binary.yaml": SkipBinary,
		"config/link.yaml":   SkipSymlink,
	}
	if len(gotSkips) != len(wantSkips) {
		t.Errorf("skipped = %v, want %v", gotSkips, wantSkips)
	}
	for p, r := range wantSkips {
		if gotSkips[p] != r {
			t.Errorf("%s: reason %q, want %q", p, gotSkips[p], r)
		}
	}

	if len(res.Roots) != 2 || !res.Roots[0].Active || !res.Roots[0].Exists || res.Roots[1].Active {
		t.Errorf("roots = %+v", res.Roots)
	}
	if res.Roots[0].Managed != 7 { // 5 files + large + binary (the symlink does not count)
		t.Errorf("managed = %d, want 7", res.Roots[0].Managed)
	}
}

func TestScanMissingRoot(t *testing.T) {
	ha, m := setup(t)
	testkit.Remove(t, ha)
	res := run(t, m, Options{})
	if res.Roots[0].Exists || !res.Roots[0].Active {
		t.Errorf("root = %+v, want active and nonexistent", res.Roots[0])
	}
}

// old moves the file into the past, outside the "racy" window.
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
		t.Fatal("an old file should end up in the cache")
	}

	second := run(t, m, Options{Cache: first.Cache})
	if f := second.Files["config/automations.yaml"]; f.Content != nil || f.OID != first.Files["config/automations.yaml"].OID {
		t.Errorf("second scan: want a cache hit with the same OID, got %+v", f)
	}

	// Same size and the same mtime restored by hand: the ctime changed, so
	// the cache cannot hide the edit.
	testkit.WriteFile(t, p, "bbbb\n")
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
	third := run(t, m, Options{Cache: second.Cache})
	f := third.Files["config/automations.yaml"]
	if f.Content == nil || string(f.Content) != "bbbb\n" || f.OID != BlobOID([]byte("bbbb\n")) {
		t.Errorf("third scan: the cache hid the edit: %+v", f)
	}
}

func TestScanDoesNotCacheRacyFiles(t *testing.T) {
	ha, m := setup(t)
	testkit.WriteFile(t, filepath.Join(ha, "automations.yaml"), "x\n")
	res := run(t, m, Options{})
	if _, ok := res.Cache["config/automations.yaml"]; ok {
		t.Error("a freshly written file should not enter the cache")
	}
	if _, ok := res.Files["config/automations.yaml"]; !ok {
		t.Error("but it should be in the result")
	}
}

func TestScanRetriesFilesThatChangeWhileRead(t *testing.T) {
	ha, m := setup(t)
	p := filepath.Join(ha, "automations.yaml")
	testkit.WriteFile(t, p, "v1\n")

	// Changes only during the first attempt: the second one reads a stable file.
	res := run(t, m, Options{afterRead: func(bp string, attempt int) {
		if bp == p && attempt == 0 {
			testkit.WriteFile(t, p, "version 2\n")
		}
	}})
	if got := string(res.Files["config/automations.yaml"].Content); got != "version 2\n" {
		t.Errorf("content = %q, want the stable one", got)
	}

	// Changes on every attempt: skipped as unstable.
	n := 0
	res = run(t, m, Options{afterRead: func(bp string, _ int) {
		if bp == p {
			n++
			testkit.WriteFile(t, p, strings.Repeat("z", n)+"\n")
		}
	}})
	if _, ok := res.Files["config/automations.yaml"]; ok {
		t.Error("an unstable file should not get in")
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != SkipUnstable {
		t.Errorf("skipped = %+v, want unstable", res.Skipped)
	}
}

func TestScanUnreadableDirIsSkippedNotDeleted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root, permissions do not block reading")
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
		t.Fatalf("skipped = %+v", res.Skipped)
	}
	if !res.Skipped[0].Covers("config/packages/a.yaml") || res.Skipped[0].Covers("config/packagesx/a.yaml") {
		t.Error("Covers does not respect the directory prefix")
	}
}

func TestReadVerified(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.yaml")
	testkit.WriteFile(t, p, "a\n")
	if _, err := ReadVerified(p, BlobOID([]byte("a\n")), maxSize); err != nil {
		t.Errorf("same content: %v", err)
	}
	if _, err := ReadVerified(p, BlobOID([]byte("b\n")), maxSize); !IsUnstable(err) {
		t.Errorf("different content: want unstable, got %v", err)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
