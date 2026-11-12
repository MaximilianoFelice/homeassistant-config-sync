package merge_test

import (
	"slices"
	"testing"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/merge"
)

func TestFollowTakesMainAndKeepsBoxHistory(t *testing.T) {
	h := newH(t)
	root := h.commit("", base)
	b := h.commit(root, map[string]string{"config/scripts.yaml": "s: box\n", "config/.storage/lovelace": "{\"v\": \"box\"}\n"})
	m := h.commit(root, map[string]string{"config/automations.yaml": "main\n"})

	res, err := merge.Follow(ctx, h.repo, b, m, merge.Options{BoxWins: lovelace})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != merge.FollowMain {
		t.Fatalf("= %+v", res)
	}
	f := h.files(res.R)
	if f["config/scripts.yaml"] != base["config/scripts.yaml"] {
		t.Errorf("the box edit must be discarded: %q", f["config/scripts.yaml"])
	}
	if f["config/automations.yaml"] != "main\n" {
		t.Errorf("main's change must be taken: %q", f["config/automations.yaml"])
	}
	if f["config/.storage/lovelace"] != "{\"v\": \"box\"}\n" {
		t.Error("push_only paths keep the box version")
	}
	if got := h.parents(res.R); !slices.Equal(got, []string{b, m}) {
		t.Errorf("parents = %v, want [box main]", got)
	}
}

func TestFollowCases(t *testing.T) {
	h := newH(t)
	root := h.commit("", base)
	if res, _ := merge.Follow(ctx, h.repo, root, "", merge.Options{}); res.Kind != merge.UpToDate || res.R != root {
		t.Errorf("no main: %+v", res)
	}
	m := h.commit(root, map[string]string{"config/scripts.yaml": "s: main\n"})
	if res, _ := merge.Follow(ctx, h.repo, root, m, merge.Options{}); res.Kind != merge.FastForwardBox || res.R != m {
		t.Errorf("box behind main: %+v", res)
	}
	// Box already has main's tree (for example right after a follow).
	same := h.commit(m, map[string]string{})
	if res, _ := merge.Follow(ctx, h.repo, same, m, merge.Options{}); res.Kind != merge.UpToDate || res.R != same {
		t.Errorf("same tree: %+v", res)
	}
	if _, err := merge.Follow(ctx, h.repo, "", m, merge.Options{}); err == nil {
		t.Error("without box it must fail")
	}
}
