package refine

import (
	"strings"

	"aigate/internal/refinesize"
)

const (
	// MaxRequestBytes is orchd's own ceiling for the encoded refine request:
	// the broker's 512 KiB cap with margin.
	MaxRequestBytes = 480 << 10
	// SelectionIDLen is the length used for the selection id while fitting
	// (real ids are 32 hex characters).
	SelectionIDLen = 64
)

// Fit drops files from the end of the selection (lowest priority: Collect
// puts README first, then the best name matches) until the request the broker
// will receive is accepted: the input bytes it counts against max_input_bytes
// (instructions + user message with per-file markers + schema, computed by
// the shared refinesize package) and the encoded request line both fit. A
// dropped file is listed as excluded with ReasonTotalCap. A selection that
// already fits is returned unchanged.
func Fit(sel Selection, repo, requirement string) Selection {
	fits := func(n int) bool {
		fs := make([]refinesize.File, n)
		for i := 0; i < n; i++ {
			fs[i] = refinesize.File{Path: sel.Contents[i].Path, Content: sel.Contents[i].Content}
		}
		return refinesize.InputBytes(requirement, fs) <= refinesize.DefaultMaxInputBytes &&
			refinesize.RequestBytes(repo, requirement, strings.Repeat("0", SelectionIDLen), fs) <= MaxRequestBytes
	}
	n := len(sel.Contents)
	if fits(n) {
		return sel
	}
	for n > 0 && !fits(n) {
		n--
	}
	out := sel
	out.Files = append([]FileEntry{}, sel.Files[:n]...)
	out.Contents = append([]File{}, sel.Contents[:n]...)
	out.Excluded = append([]Excluded{}, sel.Excluded...)
	for _, f := range sel.Contents[n:] {
		if len(out.Excluded) >= maxExcluded {
			break
		}
		p := f.Path
		if !SafeRelPath(p) {
			p = "<unsafe path>"
		}
		out.Excluded = append(out.Excluded, Excluded{Path: p, Reason: ReasonTotalCap})
	}
	out.TotalBytes = 0
	for _, f := range out.Contents {
		out.TotalBytes += len(f.Content)
	}
	out.Hash = hashOf(out.Head, out.Contents)
	return out
}
