package gitx

import (
	"slices"
	"testing"
)

// Salida real de git 2.53 (capturada a mano) para un merge con un conflicto
// archivo/directorio, uno de contenido y uno modify/delete.
const conflictOutput = "155d1bd036a34706dcd7be90f5e5cf2dcaa9c8c1\x00" +
	"dir~box\x00f.txt\x00h.txt\x00" +
	"\x00" +
	"2\x00dir~box\x00dir\x00CONFLICT (file/directory)\x00CONFLICT (file/directory): directory in the way of dir from box; moving it to dir~box instead.\n\x00" +
	"1\x00f.txt\x00Auto-merging\x00Auto-merging f.txt\n\x00" +
	"1\x00f.txt\x00CONFLICT (contents)\x00CONFLICT (content): Merge conflict in f.txt\n\x00" +
	"1\x00h.txt\x00CONFLICT (modify/delete)\x00CONFLICT (modify/delete): h.txt deleted in box and modified in main.  Version main of h.txt left in tree.\n\x00"

func TestParseMergeTreeConflicts(t *testing.T) {
	res, err := parseMergeTree([]byte(conflictOutput))
	if err != nil {
		t.Fatal(err)
	}
	if res.Tree != "155d1bd036a34706dcd7be90f5e5cf2dcaa9c8c1" {
		t.Errorf("tree = %s", res.Tree)
	}
	if !slices.Equal(res.Conflicted, []string{"dir~box", "f.txt", "h.txt"}) {
		t.Errorf("conflicted = %v", res.Conflicted)
	}
	if len(res.Messages) != 4 {
		t.Fatalf("mensajes = %+v", res.Messages)
	}
	if m := res.Messages[0]; !slices.Equal(m.Paths, []string{"dir~box", "dir"}) || m.Type != "CONFLICT (file/directory)" {
		t.Errorf("mensaje 0 = %+v", m)
	}
	if m := res.Messages[3]; m.Type != "CONFLICT (modify/delete)" || m.Message[len(m.Message)-1] == '\n' {
		t.Errorf("mensaje 3 = %+v", m)
	}
}

func TestParseMergeTreeClean(t *testing.T) {
	res, err := parseMergeTree([]byte("5803de98ce314f6b92d6d2b0e7f594fdbcedb135\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Tree != "5803de98ce314f6b92d6d2b0e7f594fdbcedb135" || len(res.Conflicted) != 0 || len(res.Messages) != 0 {
		t.Errorf("= %+v", res)
	}
}

func TestParseMergeTreeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "\x00", "abc\x00\x00x\x00", "abc\x00\x003\x00a\x00"} {
		if _, err := parseMergeTree([]byte(bad)); err == nil {
			t.Errorf("%q debería fallar", bad)
		}
	}
}
