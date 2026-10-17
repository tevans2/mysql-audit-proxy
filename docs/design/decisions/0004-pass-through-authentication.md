# ADR 0004: The proxy authenticates each user against the database

## Status

Accepted and implemented — 2026-10-16. Proposed 2026-10-06, required by the client at a
demo (24 Sep 2026) and prioritised first. The proxy change merged on 13 Oct (#75) and its
end-to-end tests on 15 Oct (#74); the [performance re-run](../../perf/block-mode-rerun.md)
measured the cost (about 2.2 ms more per connection). The decision below is unchanged from the
proposal apart from the 8 Oct amendment.

Amended 2026-10-08: decision point 4 now chooses `COM_CHANGE_USER` on a bootstrap connection
instead of a go-mysql patch, following the
[`COM_CHANGE_USER` spike](../spikes/com-change-user.md). Nothing else in the decision changed.

Does not supersede [ADR 0002](0002-go-mysql-both-halves.md) — go-mysql still speaks both halves —
but it replaces the identity model that ADR's context assumed, in which the upstream half is one
long-lived connection owned by the proxy under a configured account.

## Context

Earlier versions had the proxy authenticate clients itself, against a `PROXY_USERS` credential
store, and open every session's pinned upstream connection as one configured account. Identity
was therefore **recorded but not enforced**. Measured on `main`, through the proxy, logged in as
`alice`:

```
USER()=root@192.168.147.1   CURRENT_USER()=root@%
SELECT COUNT(*) FROM sales.orders  ->  <nil>        # succeeded; alice has no grant on it
```

Every session reaching MySQL as the same account means per-user grants do not apply, a revoked
user keeps working until someone edits `PROXY_USERS`, and the audit log is the only thing
standing between a user and any table the service account can read. This was recorded as an open
question with the client from 20 Aug and carried through the early plans.

At a client demo on 25 Sep, the client's CTO answered it: the proxy must connect to the database
**as each individual user**, with per-user upstream connections, so that identity is enforced by
MySQL rather than merely observed by us. That made it the top priority and closed the question.

The remaining question was feasibility, which issue #53 answered with a spike — a working
prototype, written up in full at
[`docs/design/spikes/pass-through-auth.md`](../spikes/pass-through-auth.md). Its verdict is go,
and its central finding reshapes the design: **the two legs do not have to be joined.** The
assumption going in was that the proxy would present the upstream's handshake to the client and
relay the whole connection phase. It does not need to. go-mysql's `server.NewServerWithAuth`
takes a custom `AuthenticationProvider` whose `Authenticate` runs inside the handshake with the
`*server.Conn` in hand, so only the **challenge and its response** need relaying. Everything
else — capability negotiation, client TLS, the command loop, prepared statements, error
propagation — stays exactly as it is today.

Verified against MySQL 8.0.46, with `spike_native` and `spike_sha2` granted `SELECT` on
`sales.customers` and nothing else:

| Connection | `USER()` | `SELECT … FROM sales.orders` |
|---|---|---|
| Direct to db-a | `spike_native@…` | `1142: SELECT command denied` |
| Through `cmd/proxy` today, as `alice` | `root@…` | succeeded |
| Through the prototype, as `spike_native` | `spike_native@…` | `1142: SELECT command denied` |
| Through the prototype, as `spike_sha2` | `spike_sha2@…` | `1142: SELECT command denied` |

The 1142 is the evidence that matters. A matching `USER()` would only prove the username was
relayed; the refusal proves MySQL is doing the enforcing.

## Decision

**The proxy performs pass-through authentication.** It relays the login exchange between the
client and the database, and the session proceeds only if the database accepts it. The proxy
holds no user credentials; `PROXY_USERS` and `parseUsers` are removed. It does hold one
powerless bootstrap credential, described under point 4.

Four points bound that decision.

1. **The relay is scoped to the authentication exchange only.** The client and upstream legs stay
   independent protocol sessions, each with its own capability negotiation and its own TLS
   termination. The proxy is explicitly *not* a byte relay — it must keep parsing SQL to audit
   it, which is the whole point of the system.

2. **The upstream connection is opened during the client's handshake**, because the username is
   not known until the client's handshake response arrives. The database's accept or reject
   decides the client's login.

3. **Proxy-to-database TLS lands first.** While the client leg is encrypted and the upstream leg
   is not, the client encodes its password in a form the upstream will not take, and the proxy
   has to open the envelope and re-seal it. With both legs encrypted every exchange becomes an
   opaque relay and that code deletes itself. Building pass-through first means writing a
   re-encoding path we intend to delete, and holding plaintext passwords in the meantime.

4. **The upstream half is `COM_CHANGE_USER` on a bootstrap connection.** `client.Conn` computes
   its own auth response from a password it holds and exposes no hook, so the proxy cannot make
   it answer a challenge that is relayed from the client. Instead the proxy opens the upstream
   with stock `client.Connect` under a bootstrap account, letting go-mysql negotiate its own
   capabilities, then sends `COM_CHANGE_USER` naming the real user and relays the resulting
   challenge to the client with the existing relay code. On OK the session *is* that user.
   - The bootstrap account needs **no privileges** — `GRANT USAGE ON *.*` is enough, and is what
     was tested — so leaking it grants nothing.
   - go-mysql defines `COM_CHANGE_USER` but implements nothing on it, so the payload encoder is
     ours: about 40 lines.
   - These connections **must never be placed in a `client.Pool`.** A pool reconnect would re-run
     the bootstrap login and hand back a connection silently running as the wrong identity.
     Sessions are pinned today, so this is a rule rather than a redesign, and it must hold.
   - A failure to log in as the bootstrap account is a proxy misconfiguration, not a user's
     failed login. It must return a generic error to the client (never the bootstrap account's
     name), log at operator level, and emit **no** `auth_fail` event.

## Consequences

**Positive**

- **MySQL enforces grants.** Verified: 1142 on an ungranted table, 1045 on a wrong password, and
  the same refusal a direct connection would give. Revocation takes effect at the database, where
  it belongs.
- **The proxy stops being a credential store for users.** `PROXY_USERS` disappears, and with it a
  file of passwords, a parser, and the question of how to rotate them. It keeps one bootstrap
  credential with no privileges (see Negative).
- **It stops needing an RSA key pair of its own.** Every go-mysql path that uses
  `serverConf.rsaPrivateKey` hangs off `DefaultAuthenticationProvider`, which our provider
  replaces, so the key is never reached. A client on a plaintext leg that asks for a public key
  gets the *upstream's*, relayed through, and encrypts something only the database can open. The
  TLS certificate is still required — without it a stock client refuses `caching_sha2_password`
  full auth with `ERROR 2061` before sending anything, and full auth is every user's first login
  after a database restart.
- **The proxy becomes plugin-agnostic.** It forwards a plugin name, a salt, and whatever bytes
  come back, without computing or verifying a hash. That supports a strictly larger set of
  plugins than today's design, where `server.Credential` has to be able to hash the password
  itself.
- **Failed logins carry the database's own code and reason**, parsed into a `*mysql.MyError` and
  returned unwrapped. The existing `auth_fail` auditing path needs no change at all.
- **User-enumeration resistance comes for free.** MySQL deliberately names an arbitrary but
  deterministic plugin for accounts that do not exist, so a prober cannot distinguish a wrong
  password from a nonexistent user. Pass-through preserves that.
- Per-user upstream connections were already the design — sessions are pinned, not pooled — so
  nothing about session handling changes.

**Negative**

- **While the legs are encrypted differently, the proxy momentarily holds a plaintext password**
  for `caching_sha2_password` full auth, and `sha256_password` accounts do not work on a TLS
  client leg at all. Both are consequences of the asymmetry, not of pass-through, and both
  disappear once point 3 above lands. Until then "the proxy never sees a password" is true for
  native password and for `caching_sha2` cache hits, and not otherwise.
- **Every login attempt opens an upstream connection before it is known to be valid.** A failed
  login now costs a TCP connect and a handshake against the database, where today it costs
  nothing past the proxy. Irrelevant at 25–50 connections; a cheap amplifier if the proxy is ever
  exposed. Wants a connection-rate limit and a deadline on the relay, neither of which blocks
  the work.
- **One extra round trip per connection on each leg** — the client is challenged twice at the
  protocol level because `server.Conn.salt` is unexported, and the upstream is opened with a
  deliberately unusable `mysql_clear_password` response so that it names the account's real
  plugin before any password is checked. Measured cost: 4.1 ms per connect against 4.3 ms direct
  and 4.0 ms through today's proxy — inside the noise, and inside the ~340 µs-per-connection
  figure quoted at the demo. That figure is for the first prototype's upstream half; with
  `COM_CHANGE_USER` the spike measured 4.53 ms against 4.10 ms for it, against 2.99 ms for a
  direct connection, so the extra round trip costs about 0.4 ms per connection, once per session.
- **The proxy holds one bootstrap credential.** It is powerless (`GRANT USAGE ON *.*`), but it
  is a secret to provision, store and rotate, and the claim that the proxy holds no credentials
  has to be stated as "no user credentials".
- **A pooled bootstrap connection would be a silent identity bug.** The no-pooling rule in
  point 4 is the only protection, so it wants a comment where the connection is opened and a
  test.
- **`client.Conn`'s internal `user` field goes stale** and keeps the bootstrap account name after
  the change. Nothing reads it today; it is a trap for a future reader.
- **The encoder assumes three capability flags** — `CLIENT_SECURE_CONNECTION`,
  `CLIENT_PLUGIN_AUTH` and `CLIENT_CONNECT_ATTRS` — which are unconditional for this go-mysql
  version against MySQL 8. A wrong flag malforms one packet and fails at login, loudly, rather
  than corrupting result sets at query time.

**Breaking**

- **Every user needs a real MySQL account.** That is the point of the ticket, but it is a
  breaking configuration change that touches `deploy/`, CI, the e2e harness, the seed data and
  the demo runsheet.
- **The account's plugin now dictates what the client must support.** Today the proxy always
  speaks `caching_sha2_password` to the client and hides the account's plugin entirely. After
  this, any account still on `mysql_native_password` becomes unreachable from a MySQL 9 client,
  which removed the plugin outright. To raise with the client before it ships.

**What this required**

The audit trail keeps recording identity exactly as it does now; what changes is that the
identity is now also the one the database sees, so `client_addr` and the audited username no
longer have to carry the whole weight of attribution. One wrinkle to document or rewrite: the
host in a relayed 1045 message is the **proxy's** address as the database saw it, not the
client's. The audit event's own `client_addr` field is still correct.

## Alternatives considered

- **Per-user upstream credentials, stored encrypted by the proxy.** Reuses the AES-256-GCM
  machinery already built for upstream passwords in #31, and is cheaper to build — perhaps 3 days
  against 5–7. Rejected: the proxy would hold every user's database password permanently rather
  than momentarily, it needs a credential-management UI and a rotation story, and it cannot
  enforce a password the database changes underneath it. It is the fallback if pass-through
  fails review.
- **Relaying the whole connection phase**, presenting the upstream's handshake to the client.
  This was the assumption in the spike brief. Rejected as both unnecessary and harmful: it would
  surrender client-leg TLS termination and capability negotiation to the upstream's choices, and
  the naive version fails outright for every `mysql_native_password` account, because the
  initial handshake advertises the *server's* default plugin rather than the account's, so the
  relayed auth-switch becomes the client's second — which real clients refuse.
- **Audit-only identity, as today.** Rejected by the client at the demo. It was a defensible
  position for an auditing tool, and it is not the one the client asked for.
- **A shim that replays a fabricated handshake to go-mysql.** This was the first prototype's
  upstream half: perform the connection phase by hand on a raw socket, then give `client.Conn` a
  `net.Conn` that replays a scripted server handshake so it believes it connected itself. It
  behaved identically to `COM_CHANGE_USER` across every login case and the command-phase checks,
  needs no bootstrap credential, and connects about 0.4 ms faster. Rejected because it copies
  go-mysql's capability set by hand and assumes the library writes its handshake response in a
  single `Write`; both assumptions fail silently, as truncated or mis-parsed result sets at
  query time. It would also be the most fragile code handed to the client. It remains in the history
  of the `spike/53-passthrough` tag.
- **A `replace` patch to go-mysql** adding an `Option` on `client.Conn` to supply the auth
  response (~20 lines), upstreamed if they will take it. This was the original point 4.
  Rejected in favour of `COM_CHANGE_USER` because it needs a fork and a `replace` directive to
  inherit and maintain, and waits on an upstream review queue.
- **Forking go-mysql wholesale, or hand-rolling the upstream command phase.** Far more code to
  own than either of the above, for the same result.
