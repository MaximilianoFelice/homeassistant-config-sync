package merge_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/merge"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

var ctx = context.Background()

// del marks a deletion in commit().
const del = "\x00delete"

type h struct {
	t    *testing.T
	repo *gitx.Repo
	n    int
}

func newH(t *testing.T) *h { return &h{t: t, repo: testkit.NewBareRepo(t)} }

// commit creates a commit on top of parent ("" = root) applying changes.
func (h *h) commit(parent string, changes map[string]string) string {
	h.t.Helper()
	base := ""
	if parent != "" {
		var err error
		if base, err = h.repo.TreeOf(ctx, parent); err != nil {
			h.t.Fatal(err)
		}
	}
	var ups []gitx.IndexUpdate
	for p, c := range changes {
		if c == del {
			ups = append(ups, gitx.IndexUpdate{Path: p})
			continue
		}
		oid, err := h.repo.WriteBlob(ctx, []byte(c))
		if err != nil {
			h.t.Fatal(err)
		}
		ups = append(ups, gitx.IndexUpdate{Path: p, Mode: "100644", OID: oid})
	}
	tree, err := h.repo.BuildTree(ctx, base, ups)
	if err != nil {
		h.t.Fatal(err)
	}
	h.n++
	id, _ := gitx.ParseIdent("Test <t@example.invalid>", time.Unix(1700000000+int64(h.n), 0))
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	c, err := h.repo.CommitTree(ctx, tree, parents, "c\n", id, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *h) merge(box, main string, opts merge.Options) *merge.Result {
	h.t.Helper()
	res, err := merge.Merge(ctx, h.repo, box, main, opts)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

// files returns a commit's content.
func (h *h) files(commit string) map[string]string {
	h.t.Helper()
	if err := h.repo.UpdateRef(ctx, "refs/heads/tmp", commit, ""); err != nil {
		// tmp already exists: overwrite it using its current value as "old".
		cur, _ := h.repo.ResolveRef(ctx, "refs/heads/tmp")
		if err := h.repo.UpdateRef(ctx, "refs/heads/tmp", commit, cur); err != nil {
			h.t.Fatal(err)
		}
	}
	return testkit.Tree(h.t, h.repo, "refs/heads/tmp")
}

func (h *h) parents(commit string) []string {
	h.t.Helper()
	// commit^N resolves the parents in order.
	var out []string
	for i := 1; ; i++ {
		p, err := h.repo.ResolveRef(ctx, commit+"^"+string(rune('0'+i)))
		if err != nil {
			h.t.Fatal(err)
		}
		if p == "" {
			return out
		}
		out = append(out, p)
	}
}

func lovelace(p string) bool { return strings.HasPrefix(p, "config/.storage/lovelace") }

var base = map[string]string{
	"README.md":                "# home\n",
	"config/automations.yaml":  "one\ntwo\nthree\nfour\nfive\n",
	"config/scripts.yaml":      "s: 1\n",
	"config/.storage/lovelace": "{\"v\": 1}\n",
}

func TestMainMissingFastForwardsMain(t *testing.T) {
	h := newH(t)
	b := h.commit("", base)
	res := h.merge(b, "", merge.Options{})
	if res.Kind != merge.FastForwardMain || res.R != b {
		t.Errorf("= %+v", res)
	}
}

func TestSameCommitIsUpToDate(t *testing.T) {
	h := newH(t)
	b := h.commit("", base)
	if res := h.merge(b, b, merge.Options{}); res.Kind != merge.UpToDate || res.R != b {
		t.Errorf("= %+v", res)
	}
}

func TestOnlyBoxChanged(t *testing.T) {
	h := newH(t)
	m := h.commit("", base)
	b := h.commit(m, map[string]string{"config/scripts.yaml": "s: 2\n"})
	if res := h.merge(b, m, merge.Options{BoxWins: lovelace}); res.Kind != merge.FastForwardMain || res.R != b {
		t.Errorf("= %+v", res)
	}
}

func TestOnlyMainChanged(t *testing.T) {
	h := newH(t)
	b := h.commit("", base)
	m := h.commit(b, map[string]string{"config/scripts.yaml": "s: 2\n"})
	res := h.merge(b, m, merge.Options{BoxWins: lovelace})
	if res.Kind != merge.FastForwardBox || res.R != m || len(res.Overridden) != 0 {
		t.Errorf("= %+v", res)
	}
}

func TestDifferentFilesMergeCleanly(t *testing.T) {
	h := newH(t)
	root := h.commit("", base)
	b := h.commit(root, map[string]string{"config/scripts.yaml": "s: box\n"})
	m := h.commit(root, map[string]string{"config/packages/new.yaml": "n: main\n", "README.md": "# home v2\n"})

	res := h.merge(b, m, merge.Options{BoxWins: lovelace})
	if res.Kind != merge.Merged || res.R == "" {
		t.Fatalf("= %+v", res)
	}
	if got := h.parents(res.R); !slices.Equal(got, []string{m, b}) {
		t.Errorf("parents = %v, want [main box] = [%s %s]", got, m, b)
	}
	f := h.files(res.R)
	if f["config/scripts.yaml"] != "s: box\n" || f["config/packages/new.yaml"] != "n: main\n" || f["README.md"] != "# home v2\n" {
		t.Errorf("tree = %v", f)
	}
}

func TestSameFileDifferentHunksMergeCleanly(t *testing.T) {
	h := newH(t)
	root := h.commit("", base)
	b := h.commit(root, map[string]string{"config/automations.yaml": "ONE\ntwo\nthree\nfour\nfive\n"})
	m := h.commit(root, map[string]string{"config/automations.yaml": "one\ntwo\nthree\nfour\nFIVE\n"})
	res := h.merge(b, m, merge.Options{})
	if res.Kind != merge.Merged {
		t.Fatalf("= %+v", res)
	}
	if got := h.files(res.R)["config/automations.yaml"]; got != "ONE\ntwo\nthree\nfour\nFIVE\n" {
		t.Errorf("automations = %q", got)
	}
}

func TestSameHunkIsAConflict(t *testing.T) {
	h := newH(t)
	root := h.commit("", base)
	b := h.commit(root, map[string]string{"config/automations.yaml": "BOX\ntwo\nthree\nfour\nfive\n", "config/scripts.yaml": "s: box\n"})
	m := h.commit(root, map[string]string{"config/automations.yaml": "MAIN\ntwo\nthree\nfour\nfive\n", "config/scripts.yaml": "s: main\n"})
	res := h.merge(b, m, merge.Options{BoxWins: lovelace})
	if res.Kind != merge.Conflict || res.R != "" {
		t.Fatalf("= %+v", res)
	}
	if !slices.Equal(res.ConflictPaths, []string{"config/automations.yaml", "config/scripts.yaml"}) {
		t.Errorf("paths = %v", res.ConflictPaths)
	}
	for _, c := range res.Conflicts {
		if c.Type != "CONFLICT (contents)" || c.Message == "" {
			t.Errorf("conflict = %+v", c)
		}
	}
}

func TestModifyDeleteIsAConflict(t *testing.T) {
	h := newH(t)
	root := h.commit("", base)
	b := h.commit(root, map[string]string{"config/scripts.yaml": del})
	m := h.commit(root, map[string]string{"config/scripts.yaml": "s: main\n"})
	res := h.merge(b, m, merge.Options{})
	if res.Kind != merge.Conflict || !slices.Equal(res.ConflictPaths, []string{"config/scripts.yaml"}) {
		t.Fatalf("= %+v", res)
	}
	if res.Conflicts[0].Type != "CONFLICT (modify/delete)" {
		t.Errorf("type = %q", res.Conflicts[0].Type)
	}
}

func TestFileDirectoryConflictNeverAutoResolves(t *testing.T) {
	h := newH(t)
	root := h.commit("", base)
	b := h.commit(root, map[string]string{"config/.storage/lovelace_x": "file\n"})
	m := h.commit(root, map[string]string{"config/.storage/lovelace_x/y": "dir\n"})
	res := h.merge(b, m, merge.Options{BoxWins: lovelace})
	if res.Kind != merge.Conflict {
		t.Fatalf("a file/directory conflict should stop even if push_only: %+v", res)
	}
}

func TestPushOnlyConflictResolvesToBox(t *testing.T) {
	h := newH(t)
	root := h.commit("", base)
	b := h.commit(root, map[string]string{"config/.storage/lovelace": "{\"v\": \"box\"}\n", "config/scripts.yaml": "s: box\n"})
	m := h.commit(root, map[string]string{"config/.storage/lovelace": "{\"v\": \"main\"}\n", "README.md": "# main\n"})
	res := h.merge(b, m, merge.Options{BoxWins: lovelace})
	if res.Kind != merge.Merged || len(res.ConflictPaths) != 0 {
		t.Fatalf("= %+v", res)
	}
	f := h.files(res.R)
	if f["config/.storage/lovelace"] != "{\"v\": \"box\"}\n" || f["config/scripts.yaml"] != "s: box\n" || f["README.md"] != "# main\n" {
		t.Errorf("tree = %v", f)
	}
	if !slices.Equal(res.Overridden, []string{"config/.storage/lovelace"}) {
		t.Errorf("overridden = %v", res.Overridden)
	}
}

func TestPushOnlyChangeOnlyInMainIsDiscarded(t *testing.T) {
	h := newH(t)
	b := h.commit("", base)
	m := h.commit(b, map[string]string{
		"config/.storage/lovelace":   "{\"v\": \"main\"}\n",
		"config/.storage/lovelace_2": "new\n",
		"config/scripts.yaml":        "s: main\n",
	})
	res := h.merge(b, m, merge.Options{BoxWins: lovelace})
	if res.Kind != merge.FastForwardBox || res.R == m {
		t.Fatalf("wanted a new commit on top of main, got %+v", res)
	}
	if got := h.parents(res.R); !slices.Equal(got, []string{m}) {
		t.Errorf("parents = %v", got)
	}
	f := h.files(res.R)
	if f["config/.storage/lovelace"] != base["config/.storage/lovelace"] {
		t.Errorf("lovelace = %q, want the box version", f["config/.storage/lovelace"])
	}
	if _, ok := f["config/.storage/lovelace_2"]; ok {
		t.Error("lovelace_2 does not exist in the box and should not remain")
	}
	if f["config/scripts.yaml"] != "s: main\n" {
		t.Error("non-push_only paths should be taken from main")
	}
	if !slices.Equal(res.Overridden, []string{"config/.storage/lovelace", "config/.storage/lovelace_2"}) {
		t.Errorf("overridden = %v", res.Overridden)
	}
}

func TestPushOnlyDeletedInMainIsRestored(t *testing.T) {
	h := newH(t)
	b := h.commit("", base)
	m := h.commit(b, map[string]string{"config/.storage/lovelace": del})
	res := h.merge(b, m, merge.Options{BoxWins: lovelace})
	if got := h.files(res.R)["config/.storage/lovelace"]; got != base["config/.storage/lovelace"] {
		t.Errorf("lovelace = %q, want it restored", got)
	}
}

func TestUnrelatedHistoriesFail(t *testing.T) {
	h := newH(t)
	b := h.commit("", map[string]string{"a": "1\n"})
	m := h.commit("", map[string]string{"b": "2\n"})
	if _, err := merge.Merge(ctx, h.repo, b, m, merge.Options{}); !errors.Is(err, gitx.ErrUnrelated) {
		t.Errorf("wanted ErrUnrelated, got %v", err)
	}
}

func TestBoxIsRequired(t *testing.T) {
	h := newH(t)
	if _, err := merge.Merge(ctx, h.repo, "", h.commit("", base), merge.Options{}); err == nil {
		t.Error("should fail without box")
	}
}
