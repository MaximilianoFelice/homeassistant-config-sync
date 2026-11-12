package apply

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

var ctx = context.Background()

type env struct {
	t       *testing.T
	ha      string
	addons  string
	repo    *gitx.Repo
	m       *paths.Matcher
	journal string
	checks  int
	verdict ha.CheckResult
	// results, si no está vacío, se devuelve en orden antes que verdict.
	results  []ha.CheckResult
	checkErr error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	ms := config.DefaultMappings()
	ms[0].Box = filepath.Join(root, "homeassistant")
	ms[1].Box = filepath.Join(root, "addon_configs")
	ms[1].Include = []string{"**/*.conf"}
	for _, mp := range ms {
		if err := os.MkdirAll(mp.Box, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m, err := paths.New(ms)
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		t: t, ha: ms[0].Box, addons: ms[1].Box,
		repo:    testkit.NewBareRepo(t),
		m:       m,
		journal: filepath.Join(root, "state", "apply.json"),
		verdict: ha.CheckResult{Valid: true},
	}
}

func (e *env) commit(parent string, changes map[string]string) string {
	return testkit.Commit(e.t, e.repo, parent, changes)
}

// boxPath traduce un path del repo al disco del env.
func (e *env) boxPath(p string) string {
	if rel, ok := strings.CutPrefix(p, "config/"); ok {
		return filepath.Join(e.ha, rel)
	}
	if rel, ok := strings.CutPrefix(p, "app_configs/"); ok {
		return filepath.Join(e.addons, rel)
	}
	return ""
}

// materialize deja en disco los archivos mapeados de un commit (disco == B).
func (e *env) materialize(commit string) {
	e.t.Helper()
	ref := "refs/heads/m"
	cur, _ := e.repo.ResolveRef(ctx, ref)
	if err := e.repo.UpdateRef(ctx, ref, commit, cur); err != nil {
		e.t.Fatal(err)
	}
	modes := testkit.Modes(e.t, e.repo, ref)
	for p, c := range testkit.Tree(e.t, e.repo, ref) {
		if bp := e.boxPath(p); bp != "" {
			perm := os.FileMode(0o644)
			if modes[p] == "100755" {
				perm = 0o755
			}
			testkit.WriteFileMode(e.t, bp, c, perm)
		}
	}
}

func (e *env) disk() map[string]string {
	out := map[string]string{}
	for k, v := range testkit.Disk(e.t, e.ha) {
		out["config/"+k] = v
	}
	for k, v := range testkit.Disk(e.t, e.addons) {
		out["app_configs/"+k] = v
	}
	return out
}

func (e *env) opts() Options {
	return Options{
		Matcher:     e.m,
		JournalPath: e.journal,
		Checker: ha.CheckerFunc(func(context.Context) (ha.CheckResult, error) {
			e.checks++
			if len(e.results) > 0 {
				r := e.results[0]
				e.results = e.results[1:]
				return r, nil
			}
			return e.verdict, e.checkErr
		}),
	}
}

func (e *env) plan(from, to string) *Plan {
	e.t.Helper()
	p, err := MakePlan(ctx, e.repo, e.m, from, to)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) journalExists() bool {
	_, err := os.Stat(e.journal)
	return err == nil
}

func assertDisk(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("disco con %d archivos, quería %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s = %q, quería %q", p, got[p], c)
		}
	}
}

var baseFiles = map[string]string{
	"README.md":                         "# casa\n",
	"config/configuration.yaml":         "homeassistant:\n",
	"config/automations.yaml":           "[]\n",
	"config/scenes.yaml":                "[]\n",
	"config/packages/viejo.yaml":        "v: 1\n",
	"config/.storage/lovelace":          "{\"v\": 1}\n",
	"app_configs/core_mosquitto/m.conf": "listener 1883\n",
}

// baseDisk es lo que materialize deja en disco para baseFiles.
func baseDisk() map[string]string {
	out := map[string]string{}
	for p, c := range baseFiles {
		if p != "README.md" {
			out[p] = c
		}
	}
	return out
}

func TestMakePlan(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	r := e.commit(b, map[string]string{
		"README.md":                         "# casa v2\n",
		"config/automations.yaml":           "- id: a\n",
		"config/scenes.yaml":                testkit.Del,
		"config/packages/nuevo.yaml":        "n: 1\n",
		"config/.storage/lovelace":          "{\"v\": 2}\n",
		"config/custom_components/x/a.yaml": "x:\n",
		"config/secrets.yaml":               "pw: 1\n",
		"config/notas.txt":                  "hola\n",
	})
	p := e.plan(b, r)

	var ops []string
	for _, op := range p.Ops {
		kind := "W"
		if op.IsDelete() {
			kind = "D"
		}
		ops = append(ops, kind+" "+op.RepoPath)
	}
	if want := []string{"D config/scenes.yaml", "W config/automations.yaml", "W config/packages/nuevo.yaml"}; !slices.Equal(ops, want) {
		t.Errorf("ops = %v, quería %v", ops, want)
	}
	ignored := map[string]string{}
	for _, ig := range p.Ignored {
		ignored[ig.RepoPath] = ig.Reason
	}
	want := map[string]string{
		"README.md":                         string(paths.ReasonOutsideMappings),
		"config/.storage/lovelace":          ReasonPushOnly,
		"config/custom_components/x/a.yaml": string(paths.ReasonExcluded),
		"config/secrets.yaml":               string(paths.ReasonHardDenied),
		"config/notas.txt":                  string(paths.ReasonNotIncluded),
	}
	if len(ignored) != len(want) {
		t.Errorf("ignorados = %v", ignored)
	}
	for k, v := range want {
		if ignored[k] != v {
			t.Errorf("%s: motivo %q, quería %q", k, ignored[k], v)
		}
	}
}

func TestApplyWritesDeletesAndChecks(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{
		"config/automations.yaml":        "- id: a\n",
		"config/packages/viejo.yaml":     testkit.Del,
		"config/packages/sub/nuevo.yaml": "n: 1\n",
		"config/python_scripts/x.py":     "x:print(1)\n",
	})
	res, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || !res.Checked || e.checks != 2 { // base + después de escribir
		t.Errorf("res = %+v, checks = %d", res, e.checks)
	}
	want := baseDisk()
	want["config/automations.yaml"] = "- id: a\n"
	delete(want, "config/packages/viejo.yaml")
	want["config/packages/sub/nuevo.yaml"] = "n: 1\n"
	want["config/python_scripts/x.py"] = "print(1)\n"
	assertDisk(t, e.disk(), want)

	if fi, err := os.Stat(filepath.Join(e.ha, "python_scripts/x.py")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("x.py debería ser ejecutable: %v %v", fi.Mode(), err)
	}
	// El journal queda en fase checked hasta que el motor mueva box.
	j, err := readJournal(e.journal)
	if err != nil || j.Phase != PhaseChecked || j.To != r {
		t.Errorf("journal = %+v, %v", j, err)
	}
	if err := Finish(e.journal); err != nil || e.journalExists() {
		t.Errorf("Finish: %v", err)
	}
	// No quedan temporales.
	for p := range testkit.Disk(t, e.ha) {
		if strings.Contains(p, ".hcs-tmp-") {
			t.Errorf("quedó un temporal: %s", p)
		}
	}
}

func TestDeletingLastFileRemovesEmptyDirs(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", map[string]string{"config/configuration.yaml": "a:\n", "config/packages/a/b/c.yaml": "c:\n"})
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/packages/a/b/c.yaml": testkit.Del})
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.ha, "packages")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("packages/ debería haberse borrado: %v", err)
	}
	if _, err := os.Stat(e.ha); err != nil {
		t.Errorf("la raíz nunca se borra: %v", err)
	}
}

func TestNoCheckWhenOnlyAddonConfigsChange(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"app_configs/core_mosquitto/m.conf": "listener 8883\n"})
	res, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked || e.checks != 0 {
		t.Errorf("no debería correr check_config: %+v", res)
	}
}

func TestPrecheckFailureTouchesNothing(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: [roto\n", "config/scenes.yaml": "ok: 1\n"})
	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrPrecheck) || !strings.Contains(err.Error(), "config/automations.yaml") {
		t.Fatalf("quería ErrPrecheck con el path, dio %v", err)
	}
	assertDisk(t, e.disk(), baseDisk())
	if e.checks != 0 || e.journalExists() {
		t.Error("no debería haber check ni journal")
	}
}

func TestRejectedConfigRollsBackExactly(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	// Permisos raros que el rollback tiene que respetar.
	if err := os.Chmod(filepath.Join(e.ha, "scenes.yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.commit(b, map[string]string{
		"config/automations.yaml":    "- id: invalida\n",
		"config/scenes.yaml":         testkit.Del,
		"config/packages/viejo.yaml": testkit.Del,
		"config/nuevo/dir/x.yaml":    "x: 1\n",
	})
	e.verdict = ha.CheckResult{Valid: false, Errors: "Invalid config for [automation]"}

	res, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "Invalid config for [automation]") {
		t.Fatalf("quería ErrRejected con los errores de HA, dio %v", err)
	}
	if !res.RolledBack || res.Applied {
		t.Errorf("res = %+v", res)
	}
	assertDisk(t, e.disk(), baseDisk())
	if fi, err := os.Stat(filepath.Join(e.ha, "scenes.yaml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("scenes.yaml restaurado con permisos %v (%v), quería 0600", fi.Mode().Perm(), err)
	}
	if _, err := os.Stat(filepath.Join(e.ha, "nuevo")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("los directorios creados por el apply tienen que desaparecer en el rollback")
	}
	if e.journalExists() {
		t.Error("después de un rollback limpio no queda journal")
	}
}

func TestWarningsPolicy(t *testing.T) {
	disabled := "Invalid config for 'input_boolean' at configuration.yaml, line 117: invalid boolean value"
	old := "Invalid config for 'timer' at configuration.yaml, line 40: offset bad"
	oldMoved := "Invalid config for 'timer' at configuration.yaml, line 42: offset bad"
	tests := []struct {
		name     string
		policy   string
		results  []ha.CheckResult // [antes, después] (o solo [después])
		rejected bool
	}{
		{"new: aparece un warning", "new", []ha.CheckResult{{Valid: true}, {Valid: true, Warnings: []string{disabled}}}, true},
		{"new: el warning ya estaba (con la línea corrida)", "new", []ha.CheckResult{{Valid: true, Warnings: []string{old}}, {Valid: true, Warnings: []string{oldMoved}}}, false},
		{"new: base inválida, después válida", "new", []ha.CheckResult{{Valid: false, Errors: "x"}, {Valid: true}}, false},
		{"any: cualquier warning", "any", []ha.CheckResult{{Valid: true, Warnings: []string{old}}}, true},
		{"ignore: solo errores", "ignore", []ha.CheckResult{{Valid: true, Warnings: []string{disabled}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			b := e.commit("", baseFiles)
			e.materialize(b)
			r := e.commit(b, map[string]string{"config/configuration.yaml": "homeassistant:\ninput_boolean:\n"})
			e.results = tt.results
			opts := e.opts()
			opts.Warnings = tt.policy
			_, err := Apply(ctx, e.repo, e.plan(b, r), opts)
			if tt.rejected {
				if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "warnings") {
					t.Fatalf("quería rechazo por warnings, dio %v", err)
				}
				assertDisk(t, e.disk(), baseDisk())
				return
			}
			if err != nil {
				t.Fatalf("no quería rechazo: %v", err)
			}
			if e.checks != len(tt.results) {
				t.Errorf("checks = %d, quería %d", e.checks, len(tt.results))
			}
		})
	}
}

func TestCheckerErrorRollsBack(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: a\n"})
	e.checkErr = errors.New("HA no responde")
	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrCheckUnavailable) {
		t.Fatalf("quería ErrCheckUnavailable, dio %v", err)
	}
	assertDisk(t, e.disk(), baseDisk())
}

func TestEditAfterSnapshotAbortsBeforeWriting(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: main\n", "config/scenes.yaml": "- s\n"})

	// Alguien edita desde la UI de HA entre el snapshot y el apply.
	testkit.WriteFile(t, filepath.Join(e.ha, "scenes.yaml"), "- editado en la UI\n")
	want := e.disk()

	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrDiskChanged) || !strings.Contains(err.Error(), "config/scenes.yaml") {
		t.Fatalf("quería ErrDiskChanged nombrando el path, dio %v", err)
	}
	assertDisk(t, e.disk(), want)
	if e.journalExists() {
		t.Error("no debería haber journal")
	}
}

func TestNewFileThatAlreadyExistsAborts(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/scripts.yaml": "s: main\n"})
	testkit.WriteFile(t, filepath.Join(e.ha, "scripts.yaml"), "s: creado en la UI\n")
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); !errors.Is(err, ErrDiskChanged) {
		t.Fatalf("quería ErrDiskChanged, dio %v", err)
	}
	if got := e.disk()["config/scripts.yaml"]; got != "s: creado en la UI\n" {
		t.Errorf("se pisó una edición: %q", got)
	}
}

func TestExecBitChangeOnDiskAborts(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: a\n"})
	if err := os.Chmod(filepath.Join(e.ha, "automations.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); !errors.Is(err, ErrDiskChanged) {
		t.Fatalf("quería ErrDiskChanged, dio %v", err)
	}
}

func TestSymlinkedParentIsRejected(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.ha, "packages2")); err != nil {
		t.Fatal(err)
	}
	r := e.commit(b, map[string]string{"config/packages2/x.yaml": "x: 1\n"})
	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("quería ErrUnsafePath, dio %v", err)
	}
	if len(testkit.Disk(t, outside)) != 0 {
		t.Error("se escribió fuera del mapeo a través del symlink")
	}
}

func TestFileBecomesDirectoryAndBack(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": testkit.Del, "config/automations.yaml/cocina.yaml": "- id: c\n"})
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); err != nil {
		t.Fatal(err)
	}
	if got := e.disk()["config/automations.yaml/cocina.yaml"]; got != "- id: c\n" {
		t.Errorf("cocina = %q", got)
	}
	_ = Finish(e.journal)

	back := e.commit(r, map[string]string{"config/automations.yaml/cocina.yaml": testkit.Del, "config/automations.yaml": "[]\n"})
	if _, err := Apply(ctx, e.repo, e.plan(r, back), e.opts()); err != nil {
		t.Fatal(err)
	}
	assertDisk(t, e.disk(), baseDisk())
}

func TestUnmanagedFileBlocksDirectoryToFileAndRollsBack(t *testing.T) {
	e := newEnv(t)
	// pk.yaml es un directorio en B y un archivo en R, pero en disco tiene
	// además un archivo no gestionado que el apply no puede borrar.
	b := e.commit("", map[string]string{"config/configuration.yaml": "a:\n", "config/pk.yaml/a.yaml": "a:\n"})
	e.materialize(b)
	testkit.WriteFile(t, filepath.Join(e.ha, "pk.yaml/notas.txt"), "no gestionado\n")
	r := e.commit(b, map[string]string{"config/pk.yaml/a.yaml": testkit.Del, "config/pk.yaml": "x: 1\n"})

	before := e.disk()
	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if err == nil || !strings.Contains(err.Error(), "no gestionado") {
		t.Fatalf("quería un error por el directorio con contenido no gestionado, dio %v", err)
	}
	assertDisk(t, e.disk(), before)
}

func TestPushOnlyIsNeverWritten(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/.storage/lovelace": "{\"v\": 99}\n"})
	res, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied || len(res.Plan.Ops) != 0 {
		t.Errorf("no debería haber operaciones: %+v", res.Plan)
	}
	assertDisk(t, e.disk(), baseDisk())
}

// TestCrashRecovery simula que el proceso muere después de cada operación
// (y después del check) y verifica que Recover deja el disco en B o en R,
// nunca mezclado.
func TestCrashRecovery(t *testing.T) {
	changes := map[string]string{
		"config/automations.yaml":           "- id: a\n",
		"config/scenes.yaml":                testkit.Del,
		"config/packages/viejo.yaml":        testkit.Del,
		"config/nuevo/x.yaml":               "x: 1\n",
		"app_configs/core_mosquitto/m.conf": "listener 8883\n",
	}
	nOps := len(changes)
	for crashAt := 0; crashAt <= nOps; crashAt++ {
		t.Run("", func(t *testing.T) {
			e := newEnv(t)
			b := e.commit("", baseFiles)
			e.materialize(b)
			r := e.commit(b, changes)
			opts := e.opts()
			opts.afterOp = func(i int) bool { return i == crashAt }

			if _, err := Apply(ctx, e.repo, e.plan(b, r), opts); !errors.Is(err, errCrash) {
				t.Fatalf("quería el crash simulado, dio %v", err)
			}
			if !e.journalExists() {
				t.Fatal("después de un crash tiene que quedar el journal")
			}

			outcome, j, err := Recover(ctx, e.repo, e.m, e.journal)
			if err != nil {
				t.Fatal(err)
			}
			// Antes del check (crashAt < nOps) o justo después del check pero
			// antes de marcar "checked": siempre se vuelve a B.
			if outcome != RecoverRolledBack || j.From != b {
				t.Errorf("outcome = %s", outcome)
			}
			assertDisk(t, e.disk(), baseDisk())
			if e.journalExists() {
				t.Error("Recover tiene que borrar el journal")
			}
		})
	}
}

func TestRecoverAfterCheckedConfirmsTarget(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: a\n"})
	// Apply completo: el journal queda en "checked" y el "proceso muere"
	// antes de que el motor mueva box.
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); err != nil {
		t.Fatal(err)
	}
	outcome, j, err := Recover(ctx, e.repo, e.m, e.journal)
	if err != nil || outcome != RecoverAtTarget || j.To != r {
		t.Fatalf("outcome = %s, %v", outcome, err)
	}
	if !e.journalExists() {
		t.Error("con at_target el journal queda hasta Finish")
	}
	if got := e.disk()["config/automations.yaml"]; got != "- id: a\n" {
		t.Errorf("automations = %q", got)
	}
}

func TestRecoverDoesNotClobberLaterEdits(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: a\n", "config/scenes.yaml": "- s\n"})
	opts := e.opts()
	opts.afterOp = func(i int) bool { return i == 1 }
	if _, err := Apply(ctx, e.repo, e.plan(b, r), opts); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	// Después del crash, alguien edita uno de los archivos.
	testkit.WriteFile(t, filepath.Join(e.ha, "scenes.yaml"), "- editado después\n")

	if _, _, err := Recover(ctx, e.repo, e.m, e.journal); err != nil {
		t.Fatal(err)
	}
	d := e.disk()
	if d["config/automations.yaml"] != "[]\n" {
		t.Errorf("automations tenía que volver a B: %q", d["config/automations.yaml"])
	}
	if d["config/scenes.yaml"] != "- editado después\n" {
		t.Errorf("la edición posterior no se tiene que pisar: %q", d["config/scenes.yaml"])
	}
}

func TestRecoverWithoutJournal(t *testing.T) {
	e := newEnv(t)
	if outcome, _, err := Recover(ctx, e.repo, e.m, e.journal); err != nil || outcome != RecoverNone {
		t.Errorf("= %s, %v", outcome, err)
	}
}
