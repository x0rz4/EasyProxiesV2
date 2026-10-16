package store

import (
	"fmt"
	"sync/atomic"
	"time"
)

// probeIDSeq is a process-wide monotonic counter appended to every generated
// probe ID. Wall-clock UnixNano has coarse granularity on some platforms
// (roughly 1 ms on Windows, up to 15.6 ms on legacy timer interrupts), so two
// probes started within the same tick would otherwise share a ProbeID and
// their results would be rejected downstream as duplicates or invalid
// replays. The monotonic suffix guarantees uniqueness within the process even
// when the clock does not advance between calls.
var probeIDSeq atomic.Uint64

// NewProbeID returns a collision-free probe identifier. The wall-clock
// timestamp keeps IDs roughly sortable and self-describing; the monotonic
// sequence makes collisions impossible within a single process. Retry paths
// that must reuse a probe ID for idempotency keep passing the original ID
// explicitly and never call this helper a second time.
func NewProbeID(prefix string) string {
	seq := probeIDSeq.Add(1)
	return fmt.Sprintf("%s:%d:%d", prefix, time.Now().UTC().UnixNano(), seq)
}
