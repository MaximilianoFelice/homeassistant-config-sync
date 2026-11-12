package gitx_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

func TestWorkTreeStageAndUpdate(t *testing.T) {
	r := testkit.NewBareRepo(t)
	dir := t.TempDir()
	index := filepath.Join(t.TempDir(), "index")
	wt := r.WorkTree(dir, index)

	testkit.WriteFile(t, filepath.Join(dir, "a.yaml"), "a: 1\n")
	testkit.WriteFile(t, filepath.Join(dir, "secret.txt"), "x\n")
	testkit.WriteFile(t, filepath.Join(dir, "*.yaml"), "a literal star\n")
	if err := os.Symlink("a.yaml", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := wt.ReadTree(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := wt.AddAll(ctx); err != nil {
		t.Fatal(err)
	}
	t1, err := wt.WriteTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	entries, _ := r.LsTree(ctx, t1)
	for _, e := range entries {
		modes[e.Path] = e.Mode
	}
	if modes["a.yaml"] != "100644" || modes["link"] != "120000" || modes["*.yaml"] != "100644" {
		t.Errorf("tree = %v", modes)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a .git appeared in the work tree")
	}

	// A path that becomes ignored leaves the index; the file stays on disk.
	testkit.WriteFile(t, filepath.Join(dir, ".gitignore"), "secret.txt\n")
	if err := wt.AddAll(ctx); err != nil {
		t.Fatal(err)
	}
	ignored, err := wt.TrackedIgnored(ctx)
	if err != nil || len(ignored) != 1 || ignored[0] != "secret.txt" {
		t.Fatalf("tracked ignored = %v, %v", ignored, err)
	}
	if err := wt.RemoveCached(ctx, ignored); err != nil {
		t.Fatal(err)
	}
	t2, _ := wt.WriteTree(ctx)
	changes, err := r.DiffTrees(ctx, t1, t2)
	if err != nil || len(changes) != 2 {
		t.Fatalf("changes = %+v, %v", changes, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.txt")); err != nil {
		t.Error("the ignored file was removed from disk")
	}

	// Update moves the work tree from t2 to t3, and refuses if a file it
	// must touch was edited meanwhile.
	blob, _ := r.WriteBlob(ctx, []byte("a: 2\n"))
	t3, err := r.BuildTree(ctx, t2, []gitx.IndexUpdate{{Path: "a.yaml", Mode: "100644", OID: blob}, {Path: "new/b.yaml", Mode: "100644", OID: blob}})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "a.yaml"), "edited on the box\n")
	if err := wt.Update(ctx, t2, t3); !errors.Is(err, gitx.ErrWorkTreeChanged) {
		t.Fatalf("wanted ErrWorkTreeChanged, got %v", err)
	}
	if got := testkit.Disk(t, dir)["a.yaml"]; got != "edited on the box\n" {
		t.Errorf("the refused update wrote anyway: %q", got)
	}
	testkit.WriteFile(t, filepath.Join(dir, "a.yaml"), "a: 1\n")
	if err := wt.AddAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := wt.Update(ctx, t2, t3); err != nil {
		t.Fatal(err)
	}
	disk := testkit.Disk(t, dir)
	if disk["a.yaml"] != "a: 2\n" || disk["new/b.yaml"] != "a: 2\n" {
		t.Errorf("disk after update = %v", disk)
	}

	// Missing and Restore bring back tracked files.
	testkit.Remove(t, filepath.Join(dir, ".gitignore"))
	missing, err := wt.Missing(ctx)
	if err != nil || len(missing) != 1 || missing[0] != ".gitignore" {
		t.Fatalf("missing = %v, %v", missing, err)
	}
	if err := wt.Restore(ctx, missing); err != nil {
		t.Fatal(err)
	}
	if testkit.Disk(t, dir)[".gitignore"] != "secret.txt\n" {
		t.Error(".gitignore was not restored")
	}
}

func TestMergeTreeOptionsAndAttributes(t *testing.T) {
	r := testkit.NewBareRepo(t)
	commit := func(parent string, files map[string]string) string {
		t.Helper()
		return testkit.Commit(t, r, parent, files)
	}
	base := commit("", map[string]string{"x.yaml": "a: 1\n", "u.txt": "l1\n"})
	box := commit(base, map[string]string{"x.yaml": "a: box\n", "u.txt": "l1\nbox\n"})
	main := commit(base, map[string]string{"x.yaml": "a: main\n", "u.txt": "l1\nmain\n"})

	res, err := r.MergeTree(ctx, box, main, nil, "")
	if err != nil || res.Clean || len(res.Conflicted) != 2 {
		t.Fatalf("plain merge = %+v, %v", res, err)
	}
	res, err = r.MergeTree(ctx, box, main, []string{"theirs"}, "")
	if err != nil || !res.Clean {
		t.Fatalf("-X theirs = %+v, %v", res, err)
	}
	blobs := treeFiles(t, r, res.Tree)
	if blobs["x.yaml"] != "a: main\n" {
		t.Errorf("-X theirs kept %q", blobs["x.yaml"])
	}

	// .gitattributes from the attribute source: u.txt is merged with union.
	attrs := commit(main, map[string]string{".gitattributes": "u.txt merge=union\n"})
	res, err = r.MergeTree(ctx, box, attrs, nil, attrs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicted) != 1 || res.Conflicted[0] != "x.yaml" {
		t.Errorf("with merge=union only x.yaml should conflict: %v", res.Conflicted)
	}
	if got := treeFiles(t, r, res.Tree)["u.txt"]; got != "l1\nbox\nmain\n" {
		t.Errorf("union merge = %q", got)
	}
}

func treeFiles(t *testing.T, r *gitx.Repo, tree string) map[string]string {
	t.Helper()
	entries, err := r.LsTree(ctx, tree)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := r.ReadBlob(ctx, e.OID)
		if err != nil {
			t.Fatal(err)
		}
		out[e.Path] = string(b)
	}
	return out
}
