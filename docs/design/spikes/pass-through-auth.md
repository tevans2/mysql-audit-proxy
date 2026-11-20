# Spike: pass-through authentication for the proxy

**Issue:** #53 — per-user identity at the database
**Tag:** `spike/53-passthrough` (not for merge; it contains both routes, this document's shim as `-mode shim`.
The spike's original branches were deleted at project close)
**Date:** 25 Sep 2026
**Status:** Superseded in part. The go/no-go verdict stands and is recorded in
[ADR 0004](../decisions/0004-pass-through-authentication.md). The upstream half described here
(the shim) was replaced by `COM_CHANGE_USER`; see
[the follow-up spike](com-change-user.md). Read this document for the evidence and the
plugin matrix, and the follow-up for the chosen design.
**Verdict:** **Go.** A working prototype authenticates as the connecting user against
MySQL 8.0.46 with both `mysql_native_password` and `caching_sha2_password`, including
`caching_sha2_password` full auth on a cold cache, and the database enforces that user's
grants.

---

## What was asked

At a client demo the client's CTO asked for the proxy to connect to the
database **as each individual user**, with per-user upstream connections, so that identity is
*enforced* by MySQL and not merely recorded in the audit log.

Today the proxy authenticates clients itself against `PROXY_USERS` and opens every session's
pinned upstream connection as one configured account. Measured on `main`, through the proxy,
logged in as `alice`:

```
go-sql-driver: USER()=root@192.168.147.1 CURRENT_USER()=root@%
SELECT COUNT(*) FROM sales.orders  ->  <nil>          # succeeded: the session is root
```

`alice` has no grant on `sales.orders`. The query succeeded anyway. That is the gap.

---

## The result, in one comparison

`spike_native` and `spike_sha2` exist on db-a with `GRANT SELECT ON sales.customers` and
nothing else. Same Go client program in every case:

| Connection | `USER()` | `SELECT … FROM sales.orders` |
|---|---|---|
| Direct to db-a | `spike_native@…` | `Error 1142: SELECT command denied … for table 'orders'` |
| Through `cmd/proxy` (main), as `alice` | `root@…` | succeeded |
| Through the prototype, as `spike_native` | `spike_native@…` | `Error 1142: SELECT command denied … for table 'orders'` |
| Through the prototype, as `spike_sha2` | `spike_sha2@…` | `Error 1142: SELECT command denied … for table 'orders'` |

The prototype is indistinguishable from a direct connection — same identity, same refusal,
same error code. `SELECT USER()` alone would only have proved the name was right; the 1142 is
what proves MySQL is doing the enforcing.

---

## How it works, and how that differs from the brief

The spike brief assumed the proxy would read the upstream's initial handshake and **present
that handshake to the client**, then relay connection-phase packets in both directions. That
is not what the prototype does, and the difference is the main architectural finding.

**Only the challenge and its response need relaying. The two legs stay separate protocol
sessions.** go-mysql's `server` package still writes its own initial handshake to the client,
with its own capability flags, its own TLS offer and its own salt. go-mysql's `client` package
still owns the command phase upstream. What changes is one step in the middle: instead of
comparing the client's password against one the proxy holds, the proxy re-challenges the
client with the *upstream's* salt and lets the database decide.

The flow:

1. Client connects. go-mysql writes its normal initial handshake and reads the response, so
   the username, capability flags, TLS upgrade and connection attributes are all parsed by the
   library exactly as today.
2. go-mysql calls our `AuthenticationProvider.Authenticate`. **This replaces the password
   comparison entirely.** The client's auth data is discarded: it answers a salt no database
   ever saw.
3. We dial the upstream and read its initial handshake.
4. We send the upstream a handshake response naming plugin `mysql_clear_password` with no
   auth data (see "The two-switch problem" below). The upstream replies with an
   AuthSwitchRequest naming the **account's** real plugin and a fresh salt.
5. We relay that as an AuthSwitchRequest to the client — its one and only plugin switch — and
   send the client's answer upstream verbatim.
6. We keep relaying until the upstream sends OK or ERR, handling `caching_sha2_password`'s
   extra exchanges on the way.
7. On OK, `Authenticate` returns `nil`. go-mysql writes the OK to the client itself and runs
   its normal command loop. On ERR we return the `*mysql.MyError` parsed from the upstream's
   ERR packet.

### The two-switch problem

The first attempt challenged the client straight away with the plugin and salt from the
upstream's initial handshake. It failed for every `mysql_native_password` account:

```
ERROR 2000 (HY000): Unknown MySQL error
auth_fail user=spike_native: read auth switch response from client: EOF
```

The reason is that a MySQL initial handshake advertises the **server's** default plugin
(`caching_sha2_password` on MySQL 8), not the plugin of the account being logged into — the
server has not read the username yet. So for a native-password account the upstream answers
our response with an auth-switch of its own, and relaying that becomes the client's *second*
switch. Real clients refuse a second switch and drop the connection.

Opening with `mysql_clear_password` and no auth data forces the upstream to name the account's
plugin up front. MySQL never lets an account be defined with that plugin, so it can never
accidentally match and be verified against our empty response. The client is then challenged
exactly once, with the right plugin and a salt the database chose. Cost: one extra round trip
on the upstream leg, on connect only.

This is the kind of thing a spike is for. A prototype that only tried `caching_sha2_password`
would have looked finished and then broken on every native-password account.

---

## Answers to the questions

### 1. Does it work at all?

Yes, for both plugins.

```
$ docker exec -e MYSQL_PWD='…' proxy-upstream-db-a \
    mysql -h host.docker.internal -P 4306 -u spike_native -e "SELECT USER(), CURRENT_USER();"
USER()                        CURRENT_USER()
spike_native@192.168.147.1    spike_native@%

$ … -u spike_sha2 -e "SELECT USER(), CURRENT_USER();"
USER()                      CURRENT_USER()
spike_sha2@192.168.147.1    spike_sha2@%
```

Grants are enforced by the database, in both directions:

```
--- granted   --- SELECT COUNT(*) FROM sales.customers      ->  3
--- denied    --- SELECT COUNT(*) FROM sales.orders         ->  ERROR 1142 (42000): SELECT command denied …
--- denied    --- DELETE FROM sales.customers               ->  ERROR 1142 (42000): DELETE command denied …
--- wrong pw  --- any                                       ->  ERROR 1045 (28000): Access denied …
--- no user   --- any                                       ->  ERROR 1045 (28000): Access denied …
```

Also verified working: a default database named in the handshake (`-D sales` →
`DATABASE()` = `sales`), `USE` mid-session, session variables, transactions, prepared
statements, the `mysql` CLI, and `go-sql-driver` — which is what the e2e harness and real
applications use.

Native-password testing used the MySQL 8.0 client inside the db-a container, because the
MySQL 9 client no longer has that plugin at all — see question 5.

### 2. The key unknown — can go-mysql's command phase take an externally-authenticated connection?

**This was the question that decided the shape of the work, and the answer is better than
hoped: the question does not arise.** The connection is never taken away from go-mysql at all.

`server.Conn` keeps every field unexported and `NewCustomizedConn` always runs its own
handshake, so there is genuinely no way to hand it a connection that is already past the
connection phase. But there is no need to. `server.NewServerWithAuth` accepts a custom
`AuthenticationProvider`, and its `Authenticate(c *server.Conn, plugin string, data []byte)`
is called **from inside** go-mysql's handshake, at exactly the right moment, with the
`*server.Conn` in hand. `server.Conn` embeds `*packet.Conn`, whose `ReadPacket` and
`WritePacket` are exported — so raw connection-phase packets can be exchanged with the client
from inside that call. Returning `nil` means "accepted", and go-mysql writes the OK and runs
`HandleCommand` as usual.

So the client leg needs **no** changes to the command loop, to `HandleQuery`,
`HandleStmtPrepare`/`Execute`/`Close`, or to error propagation.

**The upstream leg is the awkward half, not the client leg.** `client.Conn` computes its auth
response from a password it holds and exposes no hook for injecting one, and its fields are
unexported too. The prototype works around this with a shim: it performs the real connection
phase by hand on a raw socket using `packet.Conn`, then hands go-mysql's `client` package a
`net.Conn` (via the `ConnectWithDialer` hook) that **replays a scripted handshake** — a
fabricated initial handshake carrying the upstream's real capability flags, server version,
connection id and status, then a fabricated OK — before becoming a transparent pass-through.
go-mysql then lands in the command phase with correctly negotiated state, having never seen a
password.

The shim is about 80 lines and is the one piece of this I would not want to ship as-is; see
"Risks" below.

**Effect on the proxy:** smaller than expected. The code discussed here now lives
in `internal/proxy/runtime/auth.go`, `tls.go`, `session.go`, and the `handler*.go` files.

- `newHandshakeServer` → `NewServerWithAuth`, and built per connection rather than per process
  (go-mysql hangs the provider off the `Server`, not the `Conn`). The RSA key and TLS config
  are still generated once and shared, so the per-connection cost is a struct allocation.
- `newCredentialStore` / `PROXY_USERS` / `parseUsers` — **deleted**. The proxy stops holding
  credentials at all.
- `handleClient` — the upstream connection moves from the top of the function into the
  handshake, because the username is not known until the client's handshake response arrives.
  `applyPendingDB` moves from `OnAuthSuccess`'s current job into the same hook, unchanged in
  spirit; it still runs after the login is accepted and before the client's OK.
- `proxyHandler`, `watchUpstream`, `classifyErr`, the command loop, the audit events — **all
  unchanged**.

### 3. Capability flags

Because the legs are independent, the proxy does **not** mirror the upstream's flags to the
client, and there is nothing to strip. Each leg negotiates for itself:

- **Client leg** — unchanged from today. go-mysql's server advertises its own fixed set. It
  never offers `CLIENT_COMPRESS`, so a client cannot ask for compression, which is what makes
  "we can't relay compression" a non-issue rather than something to defend against.
- **Upstream leg** — the prototype must send *exactly* the capability set go-mysql's client
  package would have sent, because go-mysql records its own view of the session as
  `serverCaps & clientCaps`. If the two disagree, they disagree about the wire format.

The flag that matters is **`CLIENT_DEPRECATE_EOF`**, which decides whether result sets are
terminated by an EOF packet or an OK packet. Get it wrong and every query returns a parse
error or a truncated result. `CLIENT_CONNECT_ATTRS` also has to be set, not because the
attributes matter but because go-mysql always sends a few default ones and `SetAttributes`
merges rather than replaces, so the flag cannot be dropped. Both are pinned in a named
constant with a comment saying why.

This coupling to a private detail of go-mysql v1.16.0 is the single largest maintenance risk
in the design and is called out again below.

### 4. TLS and keys on the client leg

**TLS termination is unaffected.** The SSLRequest and TLS upgrade happen inside go-mysql's
`readHandshakeResponse`, before `Authenticate` is reached, so the library still owns it exactly
as today. No change to `newSelfSignedTLSConfig`. Verified with `tls=preferred` (the default for
both `go-sql-driver` and the `mysql` CLI) and with `--ssl-mode=DISABLED`.

**But the proxy's RSA key is now dead for authentication, and the certificate is not.** These
two are worth separating, because today's `newHandshakeServer` generates one key and uses it
for both jobs.

*The RSA key is no longer needed.* `cmd/proxy` passes a key to `server.NewServer` so that
go-mysql can run `caching_sha2_password`'s password exchange against the proxy's own key pair.
Under pass-through nothing does. Every go-mysql path that touches `serverConf.rsaPrivateKey` —
`compareSha256PasswordAuthData`, `handleCachingSha2PasswordFullAuth`, `writeAuthMoreDataPubkey`
— hangs off `DefaultAuthenticationProvider`, which `Session` replaces wholesale. When a client
on a plaintext leg does need a public key, it is handed the **upstream's**, relayed straight
through, and encrypts something only the upstream can open.

Verified by construction: the prototype passes `nil` where `cmd/proxy` passes `rsaKey`, and
every row of the plugin matrix below still passes, including `caching_sha2_password` full auth
on a cold cache over both TLS and plaintext client legs.

*The certificate is still required*, for exactly the reason recorded above `newHandshakeServer`
— unchanged by any of this. Without TLS a stock client refuses `caching_sha2_password` full
auth outright, before sending anything:

```
$ mysql … --ssl-mode=DISABLED          # cold cache, no --get-server-public-key
ERROR 2061 (HY000): Authentication plugin 'caching_sha2_password' reported error:
Authentication requires secure connection.
```

and full auth is every user's first login after a database restart. The certificate is also
still what encrypts the client leg's SQL and result data, which is the security property
demoed earlier, and go-mysql refuses to construct a `caching_sha2_password` server with
neither a key nor a certificate.

So: **one fewer key to manage, same certificate story.** go-mysql lifts an RSA key off the
certificate to satisfy its own constructor; it is simply never used. The sentence in
`newSelfSignedTLSConfig`'s comment about the RSA key being reused "since go-mysql derives the
`caching_sha2_password` RSA key from the TLS certificate automatically" becomes vestigial —
still true, no longer relevant.

### 5. Authentication plugins

**Which plugins exist.** Stock MySQL 8.0 ships three that matter over TCP:

```
$ SELECT plugin_name FROM information_schema.plugins WHERE plugin_type='AUTHENTICATION';
caching_sha2_password     <- @@default_authentication_plugin
mysql_native_password     <- removed from the MySQL 9 client; see below
sha256_password           <- not the default, rarely used
```

Others exist but are out of scope: `auth_socket` and `mysql_no_login` are not usable over TCP,
`mysql_clear_password` is client-side only and can never be an account's plugin (which is what
makes the opener trick above safe), and LDAP/Kerberos/WebAuthn are MySQL Enterprise.
MariaDB's `ed25519` is a single-round-trip challenge-response, so it should relay untouched,
but there is no MariaDB in the stack and it was not tested.

**The proxy does not implement plugins — that is the point.** Under pass-through the proxy
never computes or verifies a hash, so it does not need to understand the algorithm. It
forwards a plugin name, forwards a salt, and forwards whatever bytes come back. The plugin
list the proxy supports is therefore *not* go-mysql's list; it is "anything whose exchange is
opaque". That is a much larger set, and it is a genuine advantage over today's design, where
`server.Credential` has to be able to hash a password with a method it knows.

**The one thing that breaks the opacity is TLS asymmetry.** For `caching_sha2_password` full
auth and for `sha256_password`, what the client sends depends on whether *its* leg is
encrypted: over TLS it sends the password in the clear, over plaintext it asks for a public
key and sends ciphertext. The upstream expects whatever suits *its* leg. When the two legs
differ — which is the situation today, because the client leg is TLS and the upstream leg is
not — the proxy has to open the envelope and re-seal it.

**Measured matrix**, MySQL 8.0.46 upstream over plaintext:

| Account plugin | TLS client leg | Plaintext client leg | Relay is opaque? |
|---|---|---|---|
| `mysql_native_password` | ✅ | ✅ | yes, both |
| `mysql_native_password`, empty password | ✅ | ✅ | yes, both |
| `caching_sha2_password`, fast auth (cache hit) | ✅ | ✅ | yes, both |
| `caching_sha2_password`, full auth (cache miss) | ✅ | ✅ | **no** on TLS; yes on plaintext |
| `caching_sha2_password`, empty password | ✅ | ✅ | yes, both |
| `sha256_password` | ❌ **1045** | ✅ | yes on plaintext; **not implemented** on TLS |

Everything in that table is exercised by the prototype; the two non-obvious rows are explained
below.

**`caching_sha2_password` full auth.** The server's cache starts empty, so this is every
user's first login after a database restart — not an edge case.

- *Plaintext client leg:* pure relay. The client asks for a public key, the upstream's key
  comes back, the client encrypts against it using the salt we already relayed, and the
  ciphertext goes straight upstream. **The proxy never sees the password and needs no key of
  its own.** (This removed the proxy-side RSA key the first version of the prototype used.)
- *TLS client leg:* the client sends cleartext inside its own TLS, which the plaintext
  upstream will not accept. The proxy reads it and re-encrypts it against the upstream's
  public key. Held for the length of one function call, never logged, never stored — but held.

**`sha256_password`.** Exactly the same shape, and the plaintext case works for free through
the generic relay with no plugin-specific code at all. The TLS case fails with 1045 because
the proxy relays the client's cleartext to an upstream that wants ciphertext, and the
re-encoding path implemented for `caching_sha2_password` was not also written for this plugin
— the request byte differs (`0x01` rather than `0x02`) and the exchange is driven from the
auth-switch response rather than from an AuthMoreData. **This is unimplemented, not
impossible**; roughly 15 lines, and it is only worth writing if the client actually has
`sha256_password` accounts, which is unlikely given it is not the default.

**All of this asymmetry disappears with proxy-to-database TLS.** With both legs encrypted,
every row in that table becomes a pure relay: the client sends cleartext, the upstream accepts
cleartext, and the proxy forwards bytes it never interprets. The whole re-encoding path, the
RSA handling and the `sha256_password` gap all delete themselves.

**Recommendation, restated more strongly than before: schedule the proxy-to-database
TLS item first.** It is not just a simplification, it is what makes pass-through genuinely
password-blind.

**A note on `mysql_native_password`.** MySQL 9 removed the plugin from the *client*, so a
current `mysql` CLI cannot log into such an account through the prototype or directly:

```
ERROR 2059 (HY000): Authentication plugin 'mysql_native_password' cannot be loaded
```

That is the same fact already recorded above `newHandshakeServer`. Native-password testing
used the MySQL 8.0 client inside the db-a container. It matters for pass-through because the
*account's* plugin now dictates what the client must support — today's proxy hides that, since
it only ever speaks `caching_sha2_password` to the client. Any client account still on
`mysql_native_password` will be unreachable from a MySQL 9 client once this ships. Worth
raising with them.

### 6. Failed logins

Yes, with the database's own code and reason, and the existing auditing path needs no change.

```
auth_fail client=127.0.0.1:63905 user=spike_native:
  ERROR 1045 (28000): Access denied for user 'spike_native'@'192.168.147.1' (using password: YES)
```

The upstream's ERR packet is parsed into a `*mysql.MyError` and returned **unwrapped**, so it
survives `server.writeError`'s type assertion. `auditingAuthHandler.OnAuthFailure` already
uses `errors.As` and filters on `ER_ACCESS_DENIED_ERROR` / `ER_NO_SUCH_USER`; the upstream's
1045 matches, so `auth_fail` events keep being emitted with no change at all.

**Two wrinkles worth recording.**

- The host in the message is the **proxy's** address as the database saw it, not the client's.
  The audit event's `client_addr` field is still correct, but anyone reading the `err` text
  will see the proxy. Either rewrite the message or document it.
- MySQL deliberately names a *deterministic but arbitrary* plugin for accounts that do not
  exist — for `nosuchuser` it asked for `sha256_password` — so that a prober cannot tell a
  wrong password from a nonexistent user. Pass-through preserves that property for free, which
  is a small security win over the current design.

### 7. Effort

| Piece | Estimate | Notes |
|---|---|---|
| Native password, production quality | **2–3 days** | Client leg is nearly free. Most of it is the upstream shim, its tests, and deleting `PROXY_USERS` and everything downstream of it. |
| `caching_sha2` fast auth | **+0.5 day** | Already works; cost is tests and the auth-switch edge cases. |
| `caching_sha2` full auth | **+0.5 day with upstream TLS, +2 without** | With TLS on both legs it is a pure relay and mostly deletes code. Do it after the upstream TLS item. |
| `sha256_password` on a TLS client leg | **+0.5 day, or skip** | Only if the client has such accounts. Disappears entirely with upstream TLS. |
| Config / docs / migration | **+1 day** | `PROXY_USERS` removal, `deploy/`, `README`, `deploy/README.md`, the demo runsheet. |
| **Total** | **5–7 days** | Roughly one working week for one person. |

This assumes the shim survives review. If it does not, add 2–3 days for the alternative (see
below).

---

## Risks

**1. The upstream shim depends on unexported go-mysql behaviour.** It must send exactly the
capability set `client.writeAuthHandshake` would send, and it assumes `WritePacket` issues one
`Write` per packet. Both are true in v1.16.0; neither is a documented contract. A go-mysql
upgrade could break it silently — the failure mode is mangled result sets, not a compile
error.

*Mitigations, in order of preference:*
- **Patch go-mysql and upstream it.** The clean fix is an `Option` on `client.Conn` supplying
  the auth response — roughly 20 lines. A `replace` directive to a fork gets us there in an
  afternoon and deletes the shim entirely. **This was the recommended route at the time, and
  was not taken:** the [follow-up spike](com-change-user.md) found `COM_CHANGE_USER` needs no
  fork and no fabricated handshake, and [ADR 0004](../decisions/0004-pass-through-authentication.md)
  chose it. The `mysql_clear_password` trick is still used.
- Pin go-mysql and add an e2e test that asserts a multi-row, multi-type result set through the
  prototype, so a capability mismatch fails CI loudly.

**2. `server.Conn.salt` is unexported**, which is why the client is challenged twice at the
protocol level (go-mysql's initial handshake, then our auth-switch). It costs one client round
trip per connection. Measured, it does not matter: 4.1 ms per connect through the prototype
against 4.3 ms direct and 4.0 ms through the current proxy — within noise of both, and well
inside the ~340 µs-per-connection figure quoted at the demo staying honest.

**3. Every login now opens an upstream connection before it is known to be valid.** A failed
login costs a TCP connect and a handshake against the database, where today it costs nothing
past the proxy. At 25–50 connections this is irrelevant; it is a cheap denial-of-service
amplifier if the proxy is ever exposed. Worth a connection-rate limit, not worth blocking on.

**4. Client compatibility beyond what was tested.** Verified: `mysql` 8.0, `mysql` 9.7,
`go-sql-driver` 1.10, over both TLS and plaintext client legs. **Not verified: DBeaver/JDBC**, which is the client shown at the demo.
The relay only uses standard auth-switch and AuthMoreData packets, so it should be fine, but
it is a demo-critical path and must be tested before this ships.

**5. Behaviour change for operators.** `PROXY_USERS` disappears and every user needs a real
MySQL account. That is the point of the ticket, but it is a breaking configuration change that
touches `deploy/`, CI and the demo runsheet.

---

## Incidental finding — a real bug on `main`

Not part of this spike, but found while testing and reproducible on `main` today:

```
$ mysql -h … -P 4406 -u alice -D nosuchdb -e "SELECT 1;"
ERROR 1105 (HY000): ERROR 1049 (42000): Unknown database 'nosuchdb'
```

A client naming a bad or unpermitted default database gets **1105** instead of 1049 (or 1044).
This is the trap in `docs/known-limitations.md` at a call site nobody had checked:
`applyPendingDB` returns the error from `client.Conn.UseDB`, which wraps it with
`errors.Trace`, and `server.writeError` type-asserts on `*mysql.MyError` rather than using
`errors.As`. Fix is to unwrap before returning. **Worth its own issue.**

---

## If it had been a no-go

For the record, since the brief asked: the fallback is per-user upstream credentials stored
encrypted by the proxy, reusing the AES-256-GCM machinery already built for upstream passwords
in the back-office (#31). It is workable, and cheaper to build — perhaps 3 days — but it means
the proxy holds every user's database password permanently rather than momentarily, it needs a
credential-management UI and a rotation story, and it cannot enforce a password the database
changes underneath it. Pass-through is both better and, as it turns out, not much harder.

---

## Decision

The decision was taken and is recorded in
[ADR 0004](../decisions/0004-pass-through-authentication.md). The draft stub that stood here has
been removed so the two cannot disagree.

---

## Reproducing this

```bash
git checkout spike/53-passthrough    # a tag: detached HEAD, fine for running it
make test-env                      # starts db-a on 127.0.0.1:3306

docker exec -e MYSQL_PWD="$DB_A_ROOT_PASSWORD" proxy-upstream-db-a mysql -uroot -e "
  CREATE USER 'spike_native'@'%' IDENTIFIED WITH mysql_native_password BY 'Spike#Pass1';
  CREATE USER 'spike_sha2'@'%'   IDENTIFIED WITH caching_sha2_password BY 'Spike#Pass2';
  GRANT SELECT ON sales.customers TO 'spike_native'@'%', 'spike_sha2'@'%';"

go run ./cmd/spike-passthrough -listen 0.0.0.0:4306 -upstream 127.0.0.1:3306 -mode shim

# native password needs a client that still has the plugin (MySQL 9 removed it)
docker exec -e MYSQL_PWD='Spike#Pass1' proxy-upstream-db-a \
  mysql -h host.docker.internal -P 4306 --protocol=TCP -u spike_native \
  -e "SELECT USER(); SELECT COUNT(*) FROM sales.customers; SELECT COUNT(*) FROM sales.orders;"
```

**Spike code — throwaway, not wired into `cmd/proxy`, do not merge:**

- `internal/passthrough/upstream.go` — the upstream connection phase and the `client.Conn` shim
- `internal/passthrough/session.go` — the client leg: `AuthenticationProvider` + the relay
- `cmd/spike-passthrough/main.go` — a standalone binary

`cmd/proxy` and its existing authentication path are untouched; `go vet ./...` and
`go test ./...` pass on this branch.
