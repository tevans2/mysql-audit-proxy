package runtime

import (
	"net"
	"time"
)

// upstreamHealthCheckInterval bounds how long an idle session can hold a dead
// upstream connection open. HandleQuery/UseDB only notice a dead upstream
// when the client sends a command, so a client that goes idle right as the
// upstream disappears would otherwise never be told — the session just sits
// open with a dead upstream behind it, and no disconnect event ever fires.
const upstreamHealthCheckInterval = 3 * time.Second

// watchUpstream pings the upstream on an interval for as long as the session
// is open, so a client that is idle when the upstream disappears is still
// noticed — not just one that happens to send another command afterwards.
//
// On a failed ping it marks the session dead and closes clientConn. That
// unblocks handleClient's blocked conn.HandleCommand() read with an error,
// which sends it through the normal cleanup path: the command loop returns,
// its deferred cleanup closes the go-mysql conn and emits the disconnect
// event — the same path a client-initiated disconnect already takes.
//
// If a real command is holding upstreamMu instead, this is idle-ping logic
// doesn't apply — see the TryLock branch below for the separate hang check.
//
// stop is closed by handleClient when the session ends by any other route
// (client disconnect, a query already having caught the dead upstream), so
// (client disconnect, a query already having caught the dead upstream), so
// this goroutine never outlives the session it watches. commandTimeout is
// the overall cap on how long a single query can run, if configured.
func (h *proxyHandler) watchUpstream(clientConn net.Conn, stop <-chan struct{}, commandTimeout time.Duration) {
	ticker := time.NewTicker(upstreamHealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// TryLock, not Lock: if a real command is in flight, HandleQuery
			// or UseDB will classify any failure themselves when it returns.
			// Blocking here would just delay the ping until the connection is
			// free, defeating the point of checking while otherwise idle.
			if !h.upstreamMu.TryLock() {
				// A held lock alone means nothing — a burst of ordinary quick
				// queries contends for the same tick just as often as a
				// wedged one. commandStartedAt gives the actual age of
				// whichever command holds it right now, so only a command
				// that has genuinely been running for multiple read timeouts
				// trips this, not query frequency or one legitimately slow
				// statement.
				if startedAt := h.commandStartedAt.Load(); startedAt != 0 {
					if commandTimeout > 0 && time.Since(time.Unix(0, startedAt)) >= commandTimeout {
						// The command loop is blocked inside Execute/UseDB,
						// reading the upstream socket — nothing is reading
						// the client socket right now, so closing it here
						// would do nothing until that blocked read returns,
						// which is exactly what isn't happening.
						//
						// Do NOT call h.upstream.Close() from this goroutine:
						// *client.Conn embeds *packet.Conn, and
						// packet.Conn.Close() does its own unsynchronized
						// bookkeeping beyond the plain net.Conn close — it is
						// not safe to call concurrently with the in-flight
						// Execute/UseDB's ReadPacket/WritePacket on the same
						// *client.Conn (confirmed by -race: they share
						// unsynchronized state). Calling SetDeadline instead
						// is safe: it isn't overridden by packet.Conn, so it
						// resolves to the embedded net.Conn's SetDeadline,
						// which the net.Conn contract documents as safe to
						// call from another goroutine while a Read/Write is
						// blocked — the standard way to cancel one from
						// outside. Setting it to a time already in the past
						// trips it immediately. The blocked HandleQuery/UseDB
						// call then gets an i/o timeout, runs its own
						// classifyErr, and drives the existing cleanup path —
						// nothing further to do here. Closing the connection
						// itself is left to handleClient's own deferred
						// upstream.Close(), on the goroutine that owns it.
						h.upstream.SetDeadline(time.Now())
						return
					}
				}
				continue
			}
			// Apply a strict deadline specifically for the health-check ping
			// so it doesn't block the watcher forever if UpstreamReadTimeout is 0.
			//
			// We cannot use SetDeadline before Ping() because go-mysql's ReadPacket
			// overwrites it with UpstreamReadTimeout on every read if configured.
			// Instead, we execute Ping() in a goroutine and forcefully abort the
			// socket read from the outside if it exceeds the health check interval.
			pingDone := make(chan error, 1)
			go func() {
				pingDone <- h.upstream.Ping()
			}()

			var err error
			select {
			case err = <-pingDone:
			case <-time.After(upstreamHealthCheckInterval):
				// Ping took too long. Forcefully terminate the underlying
				// blocked ReadPacket call by setting a deadline in the past.
				h.upstream.SetDeadline(time.Now())
				err = <-pingDone // Wait for Ping() to return the timeout error
			}
			h.upstreamMu.Unlock()

			if isUpstreamLost(err) {
				h.upstreamDead.Store(true)
				clientConn.Close()
				return
			}
		}
	}
}
