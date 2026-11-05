package remote

// Everything a VM returns is untrusted input. These are the hard caps on it;
// exceeding one is an error, never a truncation and never a verdict.
const (
	DefaultJUnitBytes = 8 << 20 // JUnit XML
	DefaultJSONBytes  = 8 << 20 // go test -json stdout
	DefaultTailBytes  = 1500    // last bytes of combined output
	// frameSlack covers the fixed framing lines of a reply.
	frameSlack = 4096
)

// Limits caps what is accepted from a VM. Zero (or a value above the default)
// means the default: the caps can be tightened, not loosened.
type Limits struct {
	JUnitBytes, JSONBytes, TailBytes int
}

func (l Limits) norm() Limits {
	c := func(v, def int) int {
		if v <= 0 || v > def {
			return def
		}
		return v
	}
	return Limits{c(l.JUnitBytes, DefaultJUnitBytes), c(l.JSONBytes, DefaultJSONBytes), c(l.TailBytes, DefaultTailBytes)}
}

func (l Limits) maxResult() int { return max(l.JUnitBytes, l.JSONBytes) }

// maxReply is the most stdout bytes read from one ssh call: a result, a tail
// and the framing.
func (l Limits) maxReply() int { return l.maxResult() + l.TailBytes + frameSlack }
