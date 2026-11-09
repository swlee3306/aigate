package refine

import (
	"fmt"
	"strings"
	"testing"

	"aigate/internal/refinesize"
)

// bigSelection is what Collect would return for many large files with long
// paths: README first, then name matches.
func bigSelection(n, size, pathLen int) Selection {
	sel := Selection{Head: strings.Repeat("a", 40), Files: []FileEntry{}, Excluded: []Excluded{}}
	for i := 0; i < n; i++ {
		p := "README.md"
		if i > 0 {
			p = fmt.Sprintf("%s/f%02d.go", strings.Repeat("d", pathLen-8), i)
		}
		c := strings.Repeat("x", size)
		sel.Files = append(sel.Files, FileEntry{Path: p, Bytes: len(c)})
		sel.Contents = append(sel.Contents, File{Path: p, Content: c})
		sel.TotalBytes += len(c)
	}
	sel.Hash = hashOf(sel.Head, sel.Contents)
	return sel
}

func sizes(sel Selection, repo, req string) (int, int) {
	fs := make([]refinesize.File, len(sel.Contents))
	for i, f := range sel.Contents {
		fs[i] = refinesize.File{Path: f.Path, Content: f.Content}
	}
	return refinesize.InputBytes(req, fs), refinesize.RequestBytes(repo, req, strings.Repeat("0", SelectionIDLen), fs)
}

func TestFitTrimsLowestPriorityUntilBothCapsHold(t *testing.T) {
	req := strings.Repeat("가", 2000)
	repo := "/home/orch/repos/toy"
	sel := bigSelection(40, 6000, 200)
	in, rq := sizes(sel, repo, req)
	if in <= refinesize.DefaultMaxInputBytes {
		t.Fatalf("fixture too small to need trimming: %d", in)
	}
	got := Fit(sel, repo, req)
	in, rq = sizes(got, repo, req)
	if in > refinesize.DefaultMaxInputBytes || rq > MaxRequestBytes {
		t.Fatalf("still too big: input %d request %d", in, rq)
	}
	if len(got.Files) == 0 || got.Files[0].Path != "README.md" || len(got.Files) != len(got.Contents) {
		t.Fatalf("files %+v", got.Files)
	}
	dropped := len(sel.Files) - len(got.Files)
	if dropped == 0 || len(got.Excluded) != dropped {
		t.Fatalf("dropped %d, excluded %d", dropped, len(got.Excluded))
	}
	for _, e := range got.Excluded {
		if e.Reason != ReasonTotalCap {
			t.Fatalf("reason %q", e.Reason)
		}
	}
	// the dropped ones are the lowest priority (the tail) and the cut is minimal
	for i, f := range got.Files {
		if f.Path != sel.Files[i].Path {
			t.Fatalf("file %d changed: %s", i, f.Path)
		}
	}
	next := sel
	next.Contents = sel.Contents[:len(got.Files)+1]
	if in2, rq2 := sizes(next, repo, req); in2 <= refinesize.DefaultMaxInputBytes && rq2 <= MaxRequestBytes {
		t.Fatal("one more file would have fit: the cut is not minimal")
	}
	// bookkeeping follows the files
	total := 0
	for _, f := range got.Contents {
		total += len(f.Content)
	}
	if got.TotalBytes != total || got.Hash != hashOf(got.Head, got.Contents) || got.Hash == sel.Hash {
		t.Fatalf("total %d/%d hash %s", got.TotalBytes, total, got.Hash)
	}
	// a selection that fits is returned untouched
	small := bigSelection(3, 100, 40)
	if out := Fit(small, repo, "x"); len(out.Files) != 3 || len(out.Excluded) != 0 || out.Hash != small.Hash {
		t.Fatalf("small selection changed: %+v", out)
	}
}

func TestFitRespectsTheRequestCapWithEscapes(t *testing.T) {
	// 7 KiB of control characters escapes to 6 bytes each: the encoded request
	// is far bigger than the raw input, and must be trimmed on its own cap.
	sel := bigSelection(30, 1, 20)
	for i := range sel.Contents {
		sel.Contents[i].Content = strings.Repeat("\x01", 7000)
		sel.Files[i].Bytes = 7000
	}
	sel.Hash = hashOf(sel.Head, sel.Contents)
	got := Fit(sel, "/r", "req")
	in, rq := sizes(got, "/r", "req")
	if rq > MaxRequestBytes || in > refinesize.DefaultMaxInputBytes || len(got.Files) >= 30 {
		t.Fatalf("input %d request %d files %d", in, rq, len(got.Files))
	}
}
