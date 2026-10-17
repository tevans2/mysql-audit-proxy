# Spike: COM_CHANGE_USER as the upstream half of pass-through authentication

**Issue:** #53 — per-user identity at the database
**Tag:** `spike/53-passthrough` (commit `d6ce56e`; the spike's branch was deleted at project close; not for merge)
**Date:** 6 Oct 2026
**Verdict:** **Go.** Behaviourally identical to the shim across every case tested, on go-mysql's
public API only, with no fabricated protocol packets. **Recommend it replaces decision point 4
of [ADR 0004](../decisions/0004-pass-through-authentication.md).**

---

## What this is answering

The [pass-through spike](pass-through-auth.md) settled *whether* the proxy can authenticate each
user against the database. It left open *how* the upstream half should be built, and shipped one
answer: perform the upstream login by hand on a raw socket, then hand go-mysql's client package a
`net.Conn` replaying a **fabricated handshake** so it believes it opened the connection itself.

That works, and it is the single most fragile thing on the branch. It hand-copies go-mysql's
capability set (`upstream.go:51`) and assumes the library writes its handshake response in exactly
one `Write` (`:524`). Both assumptions fail *silently* — a capability mismatch corrupts result-set
framing at query time rather than failing the connection.

ADR 0004 point 4 proposed fixing that by patching go-mysql behind a `replace` directive. This
spike tests a third option that needs neither the fabrication nor the fork.

## The idea

`COM_CHANGE_USER` re-runs the entire authentication exchange on an already-open connection — it is
how connection pools hand a pooled connection to a different user. So:

1. open the upstream with **stock `client.Connect`** under a bootstrap account, letting go-mysql
   perform its own handshake and negotiate its own capabilities;
2. send `COM_CHANGE_USER` naming the real user, and relay the resulting challenge to the
   connecting client **using the existing relay code, unchanged**;
3. on OK the session *is* that user, and the `*client.Conn` is an ordinary object built by the
   library.

Nothing is fabricated. `client.Conn` embeds `*packet.Conn` as an **exported** field, and
`packet.Conn` exports `ReadPacket`, `WritePacket` and `ResetSequence` — which is all a command
exchange needs. `relayConnectionPhase` already handles the OK / ERR / auth-switch / AuthMoreData
dance and was reused verbatim.

go-mysql defines `mysql.COM_CHANGE_USER` (`mysql/const.go:70`) and implements nothing on top of
it, so the payload encoder is ours: 40 lines, in `changeuser.go`.

---

## Evidence

Both routes run from one binary (`-mode shim|changeuser`) so the same client drives both and the
comparison is an A/B rather than two write-ups. MySQL 8.0.46 upstream over plaintext; `spike_*`
accounts granted `SELECT` on `sales.customers` and nothing else; `lustre_bootstrap` holds
`GRANT USAGE ON *.*` — that is, no privileges at all.

### Login matrix — identical, case for case

| Case | changeuser | shim |
|---|---|---|
| `caching_sha2` full auth (cache flushed), TLS client leg | `spike_sha2@…` | `spike_sha2@…` |
| `caching_sha2` fast auth, TLS client leg | `spike_sha2@…` | `spike_sha2@…` |
| `caching_sha2` full auth, plaintext leg + `--get-server-public-key` | `spike_sha2@…` | `spike_sha2@…` |
| `mysql_native_password`, TLS client leg | `spike_native@…` | `spike_native@…` |
| `mysql_native_password`, plaintext client leg | `spike_native@…` | `spike_native@…` |
| `sha256_password`, TLS client leg | **1045** | **1045** |
| `sha256_password`, plaintext client leg | `spike_sha256@…` | `spike_sha256@…` |
| empty password (`mysql_native_password`) | `spike_empty@…` | `spike_empty@…` |
| wrong password | 1045 | 1045 |
| ungranted table | **1142** | **1142** |

The `sha256_password`-on-TLS gap is inherited, not introduced: both routes share
`relayConnectionPhase`, and the re-encoding path was only written for `caching_sha2_password`. It
disappears when the upstream leg gets TLS, exactly as the first write-up argues.

`caching_sha2` over a plaintext client leg needs the client's `--get-server-public-key`; without
it the *client* refuses with 2061 before sending anything, with or without a proxy. That is the
behaviour already recorded as the reason the TLS certificate is still required.

### The command phase is intact

This is the part that actually tests the idea, driven through `go-sql-driver` — what the e2e
harness and real applications use. The hazard is `CLIENT_DEPRECATE_EOF`: if go-mysql's view of the
negotiated capabilities diverges from what the database agreed, result sets come back truncated or
mis-parsed with **no error raised anywhere**. A multi-row, multi-type SELECT read to completion is
the canary.

```
===== CHANGEUSER (:4306) =====          ===== SHIM (:4307) =====
  USER() = spike_native@…                 USER() = spike_native@…
  ok  identity                            ok  identity
  ok  multi-row multi-type result set     ok  multi-row multi-type result set
  ok  prepared statement (binary proto)   ok  prepared statement (binary proto)
  ok  transaction (session state pinned)  ok  transaction (session state pinned)
  ok  session variables survive           ok  session variables survive
  ok  grants enforced (1142)              ok  grants enforced (1142)
```

Capabilities are negotiated by go-mysql itself on the change-user route, so this result is
expected — but it is the assumption the whole option rests on, so it is measured rather than
argued.

### Connect cost

30 fresh connections each, one machine, loopback:

| Route | mean | worst |
|---|---|---|
| Direct to MySQL | 2.99 ms | 7.2 ms |
| Shim | 4.10 ms | 10.5 ms |
| **COM_CHANGE_USER** | **4.53 ms** | 8.7 ms |

The extra round trip is real and costs **~0.4 ms per connection** against the shim, paid once per
session. At the 25–50 connections Acumen specified that is not a consideration. As with the
Sprint 2 baseline, the *difference* transfers; the absolute figures do not.

---

## What it costs

**One bootstrap credential.** The proxy needs an account to open the connection before it can
change user. It needs **no grants** — `GRANT USAGE ON *.*` is enough, which is what was tested —
so a leak of it grants nothing. But it is a credential, where the shim route needs none, and ADR
0004's "the proxy stops being a credential store" has to soften to "holds one powerless
credential, and no user credentials".

**These connections must never go into a `client.Pool`.** A pool reconnect would re-run the
*bootstrap* login and hand back a connection silently running as the wrong identity. Sessions are
pinned today, so this is a rule to write down, not a redesign — but it is a sharp edge and it
belongs in the ADR.

**`client.Conn`'s internal `user` field goes stale**, still holding the bootstrap account. Nothing
reads it today. Cosmetic, and a reader will trip over it.

---

## Findings to carry into the implementation

**A misconfigured bootstrap credential is reported badly — fix before shipping.** With a wrong
bootstrap password the client sees:

```
ERROR 1105 (HY000): bootstrap connect as "lustre_bootstrap": handleAuthResult:
  ERROR 1045 (28000): Access denied for user 'lustre_bootstrap'@'192.168.147.1' (using password: YES)
```

and the proxy logs `auth_fail user=spike_native`. Two problems, both ours and both easy:

1. **It leaks the internal bootstrap account name to any client that connects.**
2. **It would be audited as that user's failed login** when it is a proxy misconfiguration — a
   false security event in the very log the product exists to keep honest.

Bootstrap failures must be classified separately from user authentication failures: generic error
to the client, operator-level log, and **no `auth_fail` event**.

**The defects listed in the pass-through write-up still apply**, because they are in the shared
code: no deadlines on the relay loops, no bounds checking on parsed packets, the leaked upstream
connection when `applyPendingDB` fails, and no tests. The change-user route removes the shim's
two silent-corruption risks; it does not touch any of those.

**One assumption remains, and it is a much weaker one.** `buildChangeUser` encodes against three
capability flags — `CLIENT_SECURE_CONNECTION`, `CLIENT_PLUGIN_AUTH`, `CLIENT_CONNECT_ATTRS` — all
unconditional for this library against MySQL 8 (the first two are in go-mysql's base capability
set; the third follows from `client.Conn` always initialising default connection attributes). The
shim needed the *entire* set to match, including `CLIENT_DEPRECATE_EOF`, and failed silently. Here
a wrong flag malforms one packet and the server rejects it during login — loudly, immediately, at
connect rather than at query time.

---

## Recommendation

**Replace ADR 0004 decision point 4** — "the upstream auth response is injected via a patch to
go-mysql, not a shim" — with COM_CHANGE_USER on a bootstrap connection, and record the bootstrap
credential and the no-pooling rule as consequences.

It is the only option tested that needs **no fork** (nothing for Acumen to inherit at handover, no
`replace` directive, no waiting on an upstream review queue) and **no fabricated protocol state**
(go-mysql negotiates its own capabilities, so the most dangerous variable is gone). It deletes
~250 lines of delicate code — `shimConn`, `fakeInitialHandshake`, `fakeOK`, `clientCapabilities` —
in exchange for ~40 lines of packet encoder, and it reuses the relay that is already proven.

Against that: one powerless credential, one rule about pooling, and ~0.4 ms per connect.

---

## Reproducing

```bash
git checkout spike/53-passthrough          # a tag, so this is a detached HEAD; fine for running it
make test-env                                   # db-a on 127.0.0.1:3306

docker exec -e MYSQL_PWD="$DB_A_ROOT_PASSWORD" proxy-upstream-db-a mysql -uroot -e "
  CREATE USER 'spike_native'@'%' IDENTIFIED WITH mysql_native_password BY 'Spike#Pass1';
  CREATE USER 'spike_sha2'@'%'   IDENTIFIED WITH caching_sha2_password BY 'Spike#Pass2';
  GRANT SELECT ON sales.customers TO 'spike_native'@'%', 'spike_sha2'@'%';
  CREATE USER 'lustre_bootstrap'@'%' IDENTIFIED WITH caching_sha2_password BY 'Bootstrap#Pass';"

# the two routes, side by side
go run ./cmd/spike-passthrough -listen 0.0.0.0:4306 -upstream 127.0.0.1:3306 \
    -mode changeuser -bootstrap-user lustre_bootstrap -bootstrap-pass 'Bootstrap#Pass'
go run ./cmd/spike-passthrough -listen 0.0.0.0:4307 -upstream 127.0.0.1:3306 -mode shim

# command-phase checks through go-sql-driver, against either
go run ./cmd/spike-verify -addr 127.0.0.1:4306 -user spike_native -pass 'Spike#Pass1'
go run ./cmd/spike-verify -addr 127.0.0.1:4306 -user spike_native -pass 'Spike#Pass1' -connects 30
```

**Spike code — throwaway, not wired into `cmd/proxy`, do not merge:**

- `internal/passthrough/changeuser.go` — the COM_CHANGE_USER route
- `internal/passthrough/session.go` — one `ChangeUser` switch selecting between the two
- `cmd/spike-passthrough/main.go` — `-mode` flag
- `cmd/spike-verify/main.go` — the `go-sql-driver` checks and the connect measurement

`cmd/proxy` and its existing authentication path remain untouched; `gofmt`, `go vet ./...` and
`go build ./...` are clean.
