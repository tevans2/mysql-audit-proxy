/*
 * Package sink holds implementations of audit.Sink — the interface the proxy
 * emits audit events through. The batched HTTP sink that posts to
 * POST /api/events joins this package later; until then Fake is the only one.
 */
package sink

import (
	"log"

	"lustre/internal/audit"
)

// Fake logs events to stdout. Used for local runs, and as the default until the
// batched HTTP sink is wired in.
type Fake struct{}

// Compile-time check that Fake still satisfies the interface.
var _ audit.Sink = Fake{}

func (Fake) Emit(e audit.Event) {
	log.Printf("sink event: conn=%s kind=%s user=%q client=%s proxy=%d upstream=%s upstream_addr=%s at=%s",
		e.ConnID, e.Kind, e.User, e.ClientAddr, e.ProxyPort, e.Upstream, e.UpstreamAddr, e.At)
}
