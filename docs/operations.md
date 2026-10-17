# Deploying and operating

The compose file in `deploy/` is a **development** stack. This page covers what a production
deployment needs, how to run it day to day, and the settings that deserve a decision per
deployment. For the architecture see [architecture.md](architecture.md).

## Deploying to production

A production deployment needs the following; none of it is optional.

### Database side

1. **Enable TLS on every MySQL server** the proxy will front, with a certificate from your CA
   whose SAN matches the hostname the proxy will use. The proxy refuses a server without TLS;
   there is no plaintext fallback.
2. **Give every person their own MySQL account** with the grants they should have.
   Use `caching_sha2_password` (MySQL 8's default). Accounts still on `mysql_native_password`
   cannot be reached from MySQL 9 clients, which removed that plugin.
3. **Create a bootstrap account on each server** for the proxy, with `GRANT USAGE ON *.*`
   only. It must be able to log in and nothing else. The proxy never needs root.
4. **Make MySQL reachable only from the proxy** (firewall, security group or private network),
   or require client certificates on user accounts (`REQUIRE X509`). Otherwise anyone with a
   password can connect directly and leave no audit record. This is the single most important
   operational control.
5. Check `max_connections` covers your peak concurrent users: each client session holds one
   upstream connection (no pooling, by design).

### Proxy and back-office side

1. **Replace every development secret** in `deploy/.env` — the table in `deploy/README.md`
   ("Secrets") lists each one and how to generate it. In particular `API_ADMIN_PASS_HASH`
   (the dev password is `admin123`), `API_CONFIG_KEY` (32 characters; encrypts stored
   bootstrap passwords — changing it later orphans them), `API_PROXY_TOKEN`.
2. **Configure your servers** in the back-office (Servers screen): name, listen port, upstream
   host and port, the bootstrap account and password, and TLS. `deploy/upstreams.json` only
   seeds the server list on a fresh database; after that the back-office is the source of
   truth.
3. **Use `tls: verify`** for every server, and mount your CA bundle (public certificates only)
   read-only into the proxy container, setting `tls_ca_file` to its path. The sample config
   uses `skip-verify` only because stock MySQL generates untrusted certificates; `skip-verify`
   still encrypts but does not check identity. See `deploy/README.md` → "Mandatory upstream
   TLS". `make test-tls` demonstrates `verify` against CA-signed databases.
4. **Set `SITE_ADDRESS`** to the back-office's real domain. Caddy then obtains and renews a
   certificate automatically (needs DNS and ports 80/443).
5. **Publish only the proxy ports and the edge.** The API (8080) and the proxy's admin port
   (9100) must stay internal.
6. **Decide retention** (see Settings that need a decision) and set `API_RETENTION_DAYS` if you want old records removed.
7. **Back up the `api-data` volume.** It holds the audit log, the server list and the admin
   accounts.

### Configuration reference

| Where | Key settings | Documented in |
|---|---|---|
| Secrets | DB, bootstrap, admin hash, config key, proxy token | `deploy/README.md` → Secrets |
| Proxy | `API_URL`, `API_PROXY_TOKEN`, `PROXY_ADMIN_ADDR`, `PROXY_DRAIN_TIMEOUT` (30 s), `PROXY_UPSTREAM_READ_TIMEOUT` (60 s), `PROXY_COMMAND_TIMEOUT` (off), `SINK_ON_FULL` (`block`) | `cmd/proxy/README.md` |
| API | `API_CONFIG_KEY`, `API_PROXY_TOKEN`, `API_ADMIN_PASS_HASH` (first boot only; the first admin is named `admin` — compose does not pass `API_ADMIN_USER` through, so add it there to choose another name), `API_RETENTION_*` | `deploy/README.md` |
| Edge | `SITE_ADDRESS` | `deploy/README.md` → The edge |

## Operating it

| Task | How |
|---|---|
| Add, change or remove a database | Back-office → Servers. The proxy picks it up within a few seconds; the screen shows when it is applied. Sessions on a moved or removed port get up to 30 s to finish |
| Add a user | Create the MySQL account and grants on the database. Nothing to do in the back-office |
| Revoke a user | Revoke or lock in MySQL. It takes effect on their next connection |
| Add a back-office admin | Back-office → Admins (any admin can) |
| Locked out of the back-office | No reset flow: update the `admins` table directly (see `deploy/README.md` → Back-office Administrator Accounts) |
| Rotate the bootstrap password | Change it in MySQL, then re-enter it on the Servers screen |
| Check health | Proxy `GET :9100/healthz` and `GET :9100/metrics` (Prometheus text) |
| Watch for audit loss | `lustre_audit_failed_total` and `lustre_audit_dropped_total` should stay at 0; `lustre_audit_queue_depth` near 0 |
| Reseed the dev stack | `docker compose -f deploy/docker-compose.yml down -v`, then `make up` |

**Metrics exist but nothing consumes them yet.** Point Prometheus (or your monitoring) at the
proxy's admin port and alert on the failed/dropped counters.

## Settings that need a decision

These are worth deciding for each deployment:

| Decision | Current default | Recommendation |
|---|---|---|
| **Retention** — how long to keep audit records, delete or archive | Off: everything kept forever (~362 bytes per statement) | 365 days, delete. Try `API_RETENTION_DRY_RUN=true` first. ADR 0006 |
| **Masking literal values** in stored SQL (personal-data rules such as POPIA or GDPR) | Raw SQL stored, literals included. Prepared statements store `?` placeholders only | Store a normalised fingerprint for search, with raw text under tighter access. Not built |
| **SQLite or Postgres** | SQLite | Stay on SQLite until you need more than one API instance, high availability, or ~100 GB of retained data. ADR 0007 |
| **Block or drop** when auditing falls behind | `block` (audit first) | Keep `block` |
