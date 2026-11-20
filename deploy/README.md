# Deploy Setup

This folder contains the local Docker setup for running and testing the system.

## What is here

- `docker-compose.yml` — two upstream MySQL servers, a proxy per upstream, the back-office
  API, and a MySQL client container.
- `.env.example` — every password, key and token the stack needs, as development values.
  Copied to the gitignored `.env` on first use; see [Secrets](#secrets).
- `upstreams.json` — the port → upstream mapping the API seeds its `servers` table from on
  first boot. After that the API's database is the source of truth, and the proxy never reads
  this file; see [The port → upstream mapping](#the-port--upstream-mapping).
- `initdb/db-a`, `initdb/db-b` — seed schemas, run once when each server first starts.
- `Caddyfile` — the edge's routing; see [The edge](#the-edge-ui-and-api-on-one-origin).
- `Dockerfile` — builds either binary; the `CMD` build arg picks which.

## The two upstreams

The proxy fronts several databases, routed by listening port. `db-a` alone proves the
pipeline; `db-b` exists so multi-listener routing has a second real server to route to.

They are deliberately unalike. A listener pointed at the wrong upstream, or carrying the
wrong credentials, fails visibly instead of returning believable rows.

| | `db-a` | `db-b` |
|---|---|---|
| Host port | `3306` | `3316` |
| Root | `root` / `rootpass` | `root` / `rootpass-b` |
| Application user | `demo` / `demopass` · `sales_app` / `salespw` | `hr_app` / `hrpw` |
| Databases | `demo`, `sales`, `auditproxy_test` | `hr`, `auditproxy_test` |
| Tables | `sales.customers`, `sales.orders` | `hr.departments`, `hr.employees` |

These are the development values from `.env.example`. `db-a` keeps its original identity
exactly — port 3306, `root`/`rootpass`, and the `demo` database and user. `internal/proxy/runtime`'s
integration tests default to that, so changing it in `.env` means pointing them at the new
values too, or they fail or, worse, silently skip.

The `sales_app` password is set by `initdb/db-a/02-sales.sql`, not `.env`: it is seed data
for demo queries, not a credential anything in the stack uses.

The service used to be called `db`. It still answers to `db` on the compose
network via an alias, so older branches keep working, but new work should say `db-a`.

### Telling them apart

Both carry the same marker table with a different row, so one query identifies whichever
server answered:

```sql
SELECT name FROM auditproxy_test.upstream_marker;   -- 'db-a' or 'db-b'
```

That is the routing check: send it through a listener and the answer names the upstream it
actually reached. Their schemas share no table names either, so a misrouted query against
`sales.customers` or `hr.employees` errors with `Unknown database` rather than succeeding.

> **`auditproxy_test` is test data, not real metadata.** It exists only so a routing check has
> something to read: it holds no audit, configuration or customer data, nothing in the proxy
> or API depends on it, and it must never be created on, or treated as part of, a real
> upstream. It is seeded by `initdb/*/01-marker.sql` for the development stack only.

## The edge image

`web/` is baked into the edge image (`deploy/Dockerfile.edge`) rather than
bind-mounted, so a deployment carries its own copy of the UI. A change to
anything under `web/` therefore needs `make up` to rebuild the image; a
`docker compose restart edge` alone will keep serving the old copy.

Responses carry `Strict-Transport-Security`, `X-Content-Type-Options`,
`Referrer-Policy` and a `Content-Security-Policy` of
`default-src 'self'; frame-ancestors 'none'`. Nothing in `web/` may use an
inline script or style, since the CSP has no exception for either.
`make edge-check` asserts all of this against the running stack.

The default `SITE_ADDRESS` is `localhost`, so a browser that has visited the
dev stack may remember HSTS for `localhost` for a year and upgrade any plain
`http://localhost` dev server to HTTPS. Clear it in the browser's HSTS settings
(`chrome://net-internals/#hsts`) if that bites.

`index.html` and the API are served `no-store`. Static assets are served
`no-cache`, which lets the browser cache them but revalidate on every request:
the filenames carry no content hash, so a long `max-age` would leave a stale
asset in place after a deploy.

## Start just the test environment

For proxy work you usually want the databases and nothing else:

```bash
make test-env         # starts db-a and db-b, waits for healthy, prints proof of both
make test-env-check   # re-runs that check against an already-running pair
```

Expected output:

```
db-a  (127.0.0.1:3306)
  marker: db-a
  sales.customers rows: 3
db-b  (127.0.0.1:3316)
  marker: db-b
  hr.employees rows: 3
both upstreams up and distinguishable
```

## The edge: UI and API on one origin

The `edge` service (Caddy) serves the back-office UI from `../web` and forwards the browser's
API calls to the `api` service, so the page and the API share one origin: no CORS, and no API
host written into the page. `make up` starts it and `make ui` opens it.

| | |
|---|---|
| URL | `https://${SITE_ADDRESS}` — `localhost` by default |
| Local certificate | Caddy's own CA; the browser warns once. `curl` needs `-k` |
| Real domain | set `SITE_ADDRESS` in `.env`; the certificate is fetched and renewed automatically, which needs public DNS and ports 80 and 443 reachable |
| Host ports | `80`, `443` (the `api` service no longer publishes `8080`) |

Only the paths the browser uses are forwarded: `/api/login`, `/api/logout`, `/api/connections`,
`/api/statements`, `/api/servers`, `/api/admins`, `/api/actions`, `/api/summary`, plus `/healthz`.
**`/api/events`, `/api/proxy/config` and `/api/proxy/status` are deliberately not**: the audit
proxy reaches them as `api:8080` on the compose network, and `/api/events` has no
authentication of its own, so publishing it would let anyone forge audit records. Anything
else under `/api/` answers 404.

## Per-user accounts

ADR 0004 needs every user to be a real MySQL account. `initdb/db-*/03-users.sh` creates them
on first start, with passwords from `.env` (the scripts are shell, not SQL, so no password is
a literal in a committed file):

| | Account | Grants |
|---|---|---|
| `db-a` | `alice` | `SELECT` on `sales.customers` and `auditproxy_test.upstream_marker` |
| `db-b` | `bob` | `SELECT` on `hr.*` and `auditproxy_test.upstream_marker` |
| both | `proxy_bootstrap` | `USAGE` only: `SELECT 1` works, any table gives error 1142 |

All use `caching_sha2_password`. The proxy passes each client's login through to the database:
it opens the session as `proxy_bootstrap`, has the database switch it to the user that is
connecting, and relays the database's challenge to the client, so the database checks the
password and the privileges. The seeded upstream rows in `upstreams.json` therefore name
`proxy_bootstrap`, with its password from `DB_A_BOOTSTRAP_PASSWORD` / `DB_B_BOOTSTRAP_PASSWORD`;
the proxy never logs in as root. **Connect clients to 3307 and 3308, not 3306 or 3316.** Those are the databases' own published
ports for development; a client on them bypasses the proxy and nothing is audited. Clients must use TLS to the proxy (`--ssl-mode=PREFERRED`, the
`mysql` default, does; the proxy's certificate is self-signed). A user that does not exist in
the database cannot connect, so add real users there. The variables are `DB_A_ALICE_PASSWORD`, `DB_A_BOOTSTRAP_PASSWORD`, `DB_B_BOB_PASSWORD` and
`DB_B_BOOTSTRAP_PASSWORD`; avoid `'` and `\` in them. Like every initdb script this only runs
on a fresh data directory, so an existing stack needs its db containers recreated. Check an
account directly:

```bash
docker compose -f deploy/docker-compose.yml exec db-a mysql -ualice -p -e 'SELECT COUNT(*) FROM sales.orders'   # 1142
```

`e2e/accounts_test.go` creates the same accounts itself (CI has no init scripts) and checks
those grants.

## Back-office Administrator Accounts

The back-office API supports named administrator accounts, session management, and rate-limiting on admin logins.

- **First-boot bootstrapping:** On first startup, if the `admins` table is empty, the API automatically seeds the initial administrator account using the `API_ADMIN_USER` and `API_ADMIN_PASS_HASH` environment variables.
- **Password recovery & updates:** Changing `API_ADMIN_PASS_HASH` in your environment after the initial boot does not retroactively update existing database accounts. If an admin password is lost or you are locked out due to rate-limiting or disabled accounts, recovery requires executing a direct SQL update against the `admins` table to reset the `pass_hash` or re-enable the user.

The `API_ADMIN_USER` and `API_ADMIN_PASS_HASH` environment variables are only used to seed the initial administrator account on first boot. Once the system is running, any admin can reset another admin's password through the back-office UI. Note that there is no global per-username lockout for failed logins; this is an intentional design choice to prevent attackers from intentionally locking out legitimate administrators by spamming failed login attempts against known usernames.

## The port → upstream mapping

`upstreams.json` lists which listening port reaches which database, and is what the API
seeds its server list from on first boot:

| Proxy port | Upstream | Host port |
|---|---|---|
| `3307` | `db-a` | `3306` |
| `3308` | `db-b` | `3316` |

Its shape is deliberately `audit.Server` exactly, which is what `GET /api/servers` returns
as `{"servers": [...]}` and what the proxy receives from `GET /api/proxy/config`, so the same
record decodes with `encoding/json` the same way everywhere.

The API uses this file (`API_CONFIG_PATH`) to **seed** its `servers` table on first boot,
and only when that table is empty. After that the database is the source of truth: servers
added or edited through `/api/servers` survive restarts, and later changes to this file are
ignored until the volume is wiped (`down -v`) — the file is not read at all once the table has
rows, so a later edit to it cannot stop the API from starting. On a **first** boot, though, a
file that is named but unreadable, unparseable or empty makes the API refuse to start rather
than come up with an empty server list. To see what it seeded, log in and
list the servers (this endpoint needs a session, which `curl` keeps in a cookie jar):

```bash
curl -sk -c /tmp/auditproxy.jar https://localhost/api/login -d '{"username":"admin","password":"<admin password>"}'
curl -sk -b /tmp/auditproxy.jar https://localhost/api/servers
```

The file holds no passwords. The API's seeded rows get theirs from `API_UPSTREAM_PASSWORDS`
(`name:password,…`), set in `docker-compose.yml`, and the API stores them encrypted with
`API_CONFIG_KEY`.

The proxy does not read this file. In this stack it gets its servers, upstream passwords
included, from the API's `GET /api/proxy/config` (`API_URL`, authenticated with
`API_PROXY_TOKEN`), keeps polling it for changes, and reconciles its listeners to match. So
servers you add, edit, disable or delete through `/api/servers` reach the proxy on its next
poll, with no restart.

The proxy has one other, static way to be configured: `PROXY_CONFIG_FILE` (with
`PROXY_UPSTREAM_PASSWORDS` for the passwords), which loads a file once and never updates. It
is mutually exclusive with `API_PROXY_TOKEN` — set one or the other, never both, or the proxy
refuses to start with an "ambiguous config" error. This stack uses the API.

## Secrets

Nothing secret is committed. `docker-compose.yml` reads every password, key and token from
`deploy/.env`, which is gitignored; `deploy/.env.example` is the committed template. Compose
picks `.env` up automatically because it sits next to the compose file, and every reference is
`${VAR:?…}`, so a missing value stops Compose with an error naming the variable instead of
starting a service with a blank secret.

On a clean checkout there is nothing to do: `make up` (and every other target that runs Compose)
copies `.env.example` to `.env` if `.env` does not exist yet. It never overwrites an existing
`.env`. Running `docker compose` by hand needs the copy first:

```bash
cp deploy/.env.example deploy/.env
```

The example values are the well-known development ones the tests, CI and Makefile default to —
fine on a laptop, not anywhere else. To use real ones, edit `deploy/.env`:

| Variable | What it is | Generate with |
|---|---|---|
| `DB_A_ROOT_PASSWORD`, `DB_B_ROOT_PASSWORD` | Upstream root passwords. Used by the databases themselves and by the tests; the proxy never logs in as root | `openssl rand -base64 24` |
| `DB_A_BOOTSTRAP_PASSWORD`, `DB_B_BOOTSTRAP_PASSWORD` | Passwords of each database's `proxy_bootstrap` account, the login the proxy itself uses. Compose also uses them to seed the API's stored upstream passwords (`API_UPSTREAM_PASSWORDS`) on first boot, and the proxy then receives them from the API. Avoid `'`, `\` and `,` | `openssl rand -hex 16` |
| `DB_A_ALICE_PASSWORD`, `DB_B_BOB_PASSWORD` | Passwords of the `alice` and `bob` database users | `openssl rand -hex 16` |
| `DB_A_APP_PASSWORD`, `DB_B_APP_PASSWORD` | The `demo` and `hr_app` application users | `openssl rand -base64 24` |
| `API_ADMIN_PASS_HASH` | bcrypt hash of the back-office admin password | `htpasswd -nbBC 10 "" 'the-password' \| cut -d: -f2` |
| `API_CONFIG_KEY` | AES-256 key for upstream passwords stored in the API database — exactly 32 characters | `openssl rand -hex 16` |
| `API_PROXY_TOKEN` | Service token the proxy presents to `GET /api/proxy/config` and on audit ingest (`POST /api/events`); a file-mode proxy sets `SINK_TOKEN` instead | `openssl rand -hex 32` |

Things to know when changing them:

- **Single-quote every value.** Compose expands `$` inside unquoted and double-quoted values,
  which mangles a bcrypt hash (`$2a$10$…`).
- **The database passwords only apply to a fresh data directory.** MySQL reads
  `MYSQL_ROOT_PASSWORD` and `MYSQL_PASSWORD` on first start only, so an existing database keeps
  its old passwords — reseed it (see [Reseeding](#reseeding)) for new ones to take effect.
- **Changing a bootstrap password after the first boot does not reach the API.** The API only
  reads `API_UPSTREAM_PASSWORDS` to seed its `servers` table, and only while that table is empty.
  Rows already in the `api-data` volume keep the account and password they were seeded with (a
  stack created before pass-through authentication still holds `root`), so the proxy would keep
  presenting the old one. For a development stack, wipe the
  volumes (`docker compose -f deploy/docker-compose.yml down -v`) so the next boot reseeds from
  `.env`; otherwise re-enter the password for that server through the back-office (`PUT
  /api/servers`). Avoid a `,` in either bootstrap password: the seed list is comma-separated.
- **Changing `API_CONFIG_KEY` orphans stored passwords.** Upstream passwords already in the
  `api-data` volume were encrypted with the old key and can no longer be decrypted. Wipe the
  volume (`docker compose -f deploy/docker-compose.yml down -v`, which also wipes the audit log)
  or re-enter them.
- **`make routes`, `make mysql-a` and `make mysql-b` log in as `alice` (db-a) and `bob` (db-b)**
  with the passwords from `.env`. For another user, create it in the database and use
  `docker compose … exec client mysql`.
- **Tests read their own variables**, not `.env` — see [Run the proxy tests](#run-the-proxy-tests)
  and `e2e/harness_test.go` for the ones to set if you change the database passwords.

## Start the full stack

```bash
make up       # or: docker compose -f deploy/docker-compose.yml up -d --build --remove-orphans
make routes   # proves each proxy port reaches its own upstream
```

`make routes` should print:

```
  :3307 -> db-a
  :3308 -> db-b
each proxy port reaches its own upstream
```

### One proxy, two listeners

There is a single `proxy` service, opening one listener per enabled server the API reports —
db-a on 3307, db-b on 3308 with the seeded data. It is started with `API_URL` and
`API_PROXY_TOKEN`, fetches `GET /api/proxy/config`, and keeps polling it, so the mapping and the
upstream passwords live in one place, the API's database, rather than in a file beside the
proxy. It is deliberately given neither `upstreams.json` nor `PROXY_UPSTREAM_PASSWORDS`
(`PROXY_CONFIG_FILE` and `API_PROXY_TOKEN` cannot be combined). When the configuration changes,
the proxy adds, updates, moves or removes listeners to match, without a restart; a removed or
moved listener stops accepting new connections straight away and lets its existing sessions
finish (up to a drain timeout) in the background.

Earlier this was two containers, `proxy-a` and `proxy-b`, each taking a single
`PROXY_UPSTREAM_ADDR` — scaffolding for when the binary could only front one upstream per
process. The `proxy` service still answers to `proxy-a` and `proxy-b` on the compose network
(and to the original single-proxy `proxy` name), so old `mysql -h proxy-a` / `-h proxy` commands
keep working.

## Connect

Directly to either upstream (development passwords from `.env.example`):

```bash
mysql -h 127.0.0.1 -P 3306 -u root -prootpass   demo   # db-a
mysql -h 127.0.0.1 -P 3316 -u hr_app -phrpw     hr     # db-b
```

Through the proxies — the port picks the database. Use the client container:

```bash
make mysql-a   # an interactive shell on db-a as alice, through the proxy on 3307
make mysql-b   # the same for db-b on 3308, as bob

# or a one-off query
docker compose -f deploy/docker-compose.yml exec client \
  mysql -h proxy-a -P 3307 -u alice -p"$DB_A_ALICE_PASSWORD" -e "SELECT name FROM auditproxy_test.upstream_marker;"
```

Each proxy passes the login through to its own database, so `alice` works through
`proxy-a` only and `bob` through `proxy-b` only; the audit log records who connected and
which `upstream` they reached.

The proxy offers `caching_sha2_password` — MySQL 8's own default authentication plugin since
8.0.4 — so a current `mysql` client connects with no `--default-auth`, `--get-server-public-key`,
or `allowPublicKeyRetrieval` override needed, the "MySQL 8 default authentication" case. It no longer matters that MySQL 9.x removed
`mysql_native_password` outright; a Homebrew MySQL 9 client that used to fail client-side before
reaching the proxy at all now authenticates normally, and the `mysql:8.0` client container above
connects the same way it always did. The client container remains available for testing
regardless.

This works with no extra flags because the proxy also terminates TLS for the client connection,
using a self-signed certificate it generates in memory on every startup — nothing to configure or
mount. A default client's `ssl-mode=PREFERRED` auto-negotiates that TLS without being asked, and
once the handshake is happening over an encrypted channel, `caching_sha2_password`'s password
exchange goes through it directly instead of needing the RSA-public-key opt-in a client otherwise
refuses to make over plaintext. Without TLS, that refusal shows up as
`ERROR 2061 (HY000): Authentication plugin 'caching_sha2_password' reported error: Authentication requires secure connection.`
and — because the client disconnects before sending a password at all — a wrong password in that
state would never reach the failed-login audit log either. The certificate isn't meant to prove
the proxy's identity (nothing here asks a client to check it, and none currently do); it exists
only so TLS can start.

The proxy logs an audit event per connection:

```bash
docker compose -f deploy/docker-compose.yml logs proxy | grep "sink event"
```

## The back-office API

```bash
curl -sk https://localhost/healthz
```

Its SQLite database lives on the `api-data` volume, so it survives a rebuild.
`docker compose -f deploy/docker-compose.yml down -v` wipes it.

## Run the proxy tests

The proxy integration tests need `db-a` running, and skip themselves silently if it is not
reachable — so a green run with nothing up covers far less than it looks like:

```bash
make test-env
go test -v -count=1 ./internal/proxy/...
```

Point them elsewhere with `PROXY_TEST_UPSTREAM_ADDR`, `_USER` and `_PASS`.

## Reseeding

The seed scripts in `initdb/` run only when a server's data directory is empty, so editing
them does nothing to a database that already exists. To pick up changes:

```bash
docker compose -f deploy/docker-compose.yml down -v   # also wipes the audit log
make test-env
```

## Services

- `db-a`, `db-b` — upstream MySQL 8.0 servers (`mysql_native_password` auth).
- `proxy` — the auditing proxy, listening on 3307 (→ `db-a`) and 3308 (→ `db-b`) as the API's live server list says. Its admin port (9100, `/healthz` and `/metrics`) is reachable on the compose network only and is not published.
- `api` — the back-office API on 8080, storing to the `api-data` volume.
- `client` — a MySQL client container for manual testing inside the Docker network.

`api` and `proxy` both have healthchecks (`docker compose ps` shows them), and `proxy` starts only
once `api` is healthy. The proxy reports healthy only when its configuration is applied and every
listener is bound, so allow up to a minute on first start. To read the audit counters, including the
drop count, from inside the network:

```bash
docker compose -f deploy/docker-compose.yml exec proxy curl -s http://127.0.0.1:9100/metrics
```

See `cmd/proxy/README.md` for proxy configuration and env var details.

## Mandatory upstream TLS

Every proxy-to-database connection now requires TLS, including the prototype-derived
bootstrap connector used before `COM_CHANGE_USER`. There is no plaintext fallback.
TLS must be available on the database before deploying this version.

Each server in `upstreams.json` or the configuration API accepts:

```json
{
  "tls": "verify",
  "tls_ca_file": "/etc/auditproxy/certs/database-ca.pem"
}
```

These fields belong alongside the existing host, port and credential fields.
Omitting `tls` means `verify`. It verifies the certificate chain, expiry and the
DNS name or IP in `upstream_host`, with TLS 1.2 or newer. An omitted or empty
`tls_ca_file` uses the proxy's system trust roots. A supplied PEM CA bundle
replaces those roots for that server; it must contain the issuing CA certificates.
An unreadable file or a bundle without valid certificates stops proxy startup.

Mount the CA bundle read-only **inside the proxy container**, for example under
`services.proxy.volumes` in a Compose override:

```yaml
- ./certs/database-ca.pem:/etc/auditproxy/certs/database-ca.pem:ro
```

The container's user (UID 10001) must be able to read it. This is a public CA
certificate bundle; do not supply a CA private key. The API only stores the path
and does not need the file mounted. Use that container path in both file and API
configuration. With API-served configuration, a change to `tls`, `tls_ca_file` or the
upstream host applies to new connections without a restart; the new settings are validated
first, and an invalid one (an unreadable CA file, say) is rejected while the current settings
keep serving. The CA bundle is read when a change is applied, so replacing the file's contents
alone is not noticed until the next configuration change or a proxy restart. The static
file configuration is read once, so restart the proxy after changing it.

Certificates need SANs matching the configured host: `db-a`/`db-b` inside Compose,
or an IP SAN such as `127.0.0.1` when the proxy runs on the host. A correct CA does
not bypass hostname verification.

`skip-verify` still requires TLS, but disables certificate authentication and emits
a startup warning for every affected server. It is for development only. The
committed **development** `upstreams.json` explicitly selects it because stock
MySQL generates certificates that are not trusted by the proxy. Production should
set `verify` and mount the appropriate CA. Existing database rows migrate to
`verify`; old API configurations are not silently relaxed to match the demo file.

The client receives a sanitised TLS failure; the proxy log identifies the server
and detailed cause. For certificate failures, check the CA, certificate dates and
SANs. For “does not offer TLS”, enable TLS on MySQL; changing to `skip-verify` will
not help. TLS failures do not emit a user `auth_fail` event.

### TLS acceptance environment

An isolated stack supplies two CA-signed MySQL servers on host ports 23306/23316
and a TLS-disabled server on 23326. It leaves the ordinary development stack alone.
Generate a disposable password and run:

```bash
export TLS_TEST_PASSWORD=$(openssl rand -hex 16)
make test-tls
make test-tls-down
```

`make test-tls` generates disposable certificates, waits for all three databases,
and runs required tests that **fail**, rather than skip, if a fixture is missing.
It checks `SHOW STATUS LIKE 'Ssl_cipher'` through both proxy routes using file and
API configuration, rejects an untrusted certificate and a TLS-disabled database,
and verifies TLS before/after the prototype's `COM_CHANGE_USER` with native,
cold-cache caching SHA-2 and SHA-256 authentication. The development exception is
also checked: `skip-verify` connects encrypted, but cannot connect without TLS.

Generated certificates/private keys are ignored by Git and expire after seven days.
To renew them, stop this disposable stack, remove `deploy/tls-test/certs`, and rerun
the target. Keep the same password while reusing the stack; remove its disposable
volumes before changing it. These test keys must never be used for deployment.

The older regression harnesses use explicit `skip-verify` with stock MySQL fixtures.
To run them with verification, set `PROXY_TEST_TLS=verify` and
`PROXY_TEST_TLS_CA_FILE` for proxy integration tests, or `E2E_TLS=verify` and
`E2E_TLS_CA_FILE` for the general e2e suite, using a CA path visible to the host tests.

### Audit-log retention

**Retention is off by default: the API deletes nothing unless you turn it on.** Deleting audit
records cannot be undone, and the client has not told us how long to keep them (see
[ADR 0006](../docs/design/decisions/0006-audit-log-retention.md)). We recommend **365 days with
`delete`**; to use it, put `API_RETENTION_DAYS=365` in `deploy/.env`. Without that setting the
log grows for ever.

| Variable | Default | What it does |
|---|---|---|
| `API_RETENTION_DAYS` | `0` (off) | Days to keep records. `0` keeps everything, for ever. Recommended: `365`. |
| `API_RETENTION_ACTION` | `delete` | `delete`, or `archive` to write expired records to files first and then delete them. |
| `API_RETENTION_ARCHIVE_DIR` | `/data/archive` | Where archive files go (only used with `archive`). In this compose file that is inside the `api-data` volume: use a separate volume for anything real. |
| `API_RETENTION_INTERVAL` | `1h` | How often the purge runs (minimum `1m`). |
| `API_RETENTION_DRY_RUN` | `false` | `true` only logs what would be removed. Nothing is deleted and no archive file is written. |

Nothing is purged when the API starts. The first purge is one interval later, so restarting the API
never deletes anything by itself. On startup the API logs how many records are already past the
cutoff and when the first run will be:

```
docker compose -f deploy/docker-compose.yml logs api | grep retention
```

Try a policy before you trust it: set `API_RETENTION_DAYS=365` and `API_RETENTION_DRY_RUN=true`,
restart, and read the log. Each run then says `dry run: would have deleted N statement(s) and M
connection(s)` and changes nothing. Remove `API_RETENTION_DRY_RUN` (or set it to `false`) to
let it delete.

An invalid value stops the API at startup. The archive files are gzip-compressed JSON Lines and
contain raw SQL, so treat the directory like the database.