# Lustre back office (web UI)

Back-office web UI, gated by an admin login. Three screens:

- **Activity** - captured **sessions** (connections) and **statements**, as two
  tabs of one panel with live counts, under a filter bar they share. Filter by
  user and time range (both tabs), by SQL text or statement type (statements),
  and by server or failed logins only (sessions). Active filters show as
  removable chips with Clear all. Click a connection to narrow the statements
  to that session. Statement kinds are risk-coded, errors appear under the
  statement they came from, and the schema each one ran against is shown.
- **Servers** - the configured port to upstream mappings, one card each,
  showing the route a client takes (the port they connect to, and the host and
  port it forwards to), the TLS setting, and whether that server is accepting
  connections. Add and edit open in a panel beside the list; deleting asks
  first and says which port will stop working. A banner above the cards gives
  the saved revision, the revision the proxy is actually running, and when it
  last reported. Live-only.
- **Admins** - who can sign in to the back office. List, create, disable and
  reset passwords, with "You" and "Disabled" tags on the accounts and the time
  each one last signed in. Disabling and resetting both ask first, in a dialog
  that says what the change does: either one ends that admin's sessions, so
  resetting your own password signs you out. Underneath, the change history
  reads as sentences and can be narrowed to server changes or admin changes.
  Live-only.

The Activity screen **updates live** by polling the API on an interval; a
Live/Paused toggle stops and resumes it, and the current filter is preserved
across updates. Both tables are fetched on every poll whichever tab is open,
because the tab labels carry live counts for both.

The filters and the open tab live in the query string, so a view can be linked
to and "Copy link to this view" copies that URL. The URL is replaced rather
than pushed: every keystroke in the search would otherwise be a history entry.
A preset time range is stored as the preset rather than the timestamps it
resolves to, so a link still means "the last 24 hours" when someone opens it
tomorrow; only a custom range pins actual times. A preset is also a rolling
window with a start and no end, resolved again for every poll, so a live view of
"the last hour" keeps showing new rows instead of freezing at the moment it was
chosen. No preset is selected by
default, because opening an audit log on the last 24 hours shows nothing
whenever nothing happened today.

Statement kinds are grouped by risk in `stmtkind.js`: reads are quiet, writes
are warm, and schema or privilege changes are outlined. Transaction and session
control is a fourth group shown neutrally, since a COMMIT drawn as a schema
change would cry wolf.

The attention tiles, the errors-only toggle, the grouped kind filter, the
server filter on statements and open-sessions-only are not here yet. Each needs
an API parameter that does not exist, and they are tracked by #83.

The header carries a pill saying whether the proxy is running the configuration
that was saved, so that answer is visible from every screen rather than only
from Servers. It reads the same four states as the Servers banner (unknown,
pending, stale, live) from `configstate.js`, and polls every 10 seconds.

The change history asks the API for the most recent 200 changes and filters
them in the browser, because `GET /api/actions` pages but has no filter of its
own. The line above the table says how many are shown against how many exist,
so a truncated log does not read as the whole history.

Pressing `/` anywhere outside a text field jumps to the SQL search.

## Filtering and paging

Filtering happens in the API, not the browser, so a filter applies to the whole
log rather than to the rows already on screen. Both tables page at 50 rows (the
API's own default; its ceiling is 500), and changing any filter returns to the
first page.

Matching is exact for user, server, statement type and connection id — `roo`
does not match `root`. Only the SQL text search is a substring match. The time
range filters connections on when they started and statements on when they ran,
so a long session's statements can fall inside a range the connection itself
does not.

The "hide client handshake" toggle is applied by the API (`hide_handshake`)
before it paginates, so a page is always full of real statements rather than
padded out with startup noise.

The SQL column shows each statement without the comment a driver or tool put in
front of it (`/* ApplicationName=DBeaver … */`), clamped to two lines. Clicking
a statement, or pressing Enter or Space on it, shows the full text exactly as
it was received. This is display only: the stored SQL and search are unchanged,
and the type badge is taken from the statement's keyword (`stripLeadingComments`
and `statementType` in `format.js`, mirroring `skipLeadingComments` in
`internal/proxy/runtime/sql.go`). `make web-test` runs the helpers' tests (it needs node 22 or newer, and so does
`make test`, which runs it first).

## Session details

Clicking a connection row, or a connection id in the statements table, opens a
slide-over with that session's details and a timeline of everything that ran in
it, oldest first. It is one request to `GET /api/connections/{id}`, which
returns the connection and its statements together, so the drawer never has to
reconcile two pages against each other.

Handshake statements are kept here even when the toggle hides them in the main
table: a session's own timeline is the one place where the statement count
beside it has to add up. The drawer also offers to filter the log to that
session, which is the question a row most often raises.

It is a native `<dialog>` opened with `showModal()`, so Esc, the backdrop and
focus handling are the browser's job, as with the confirm dialogs.

## Live updates

Both tables poll every five seconds, but polling and rendering are separate.
New rows are held back while the operator has paused, or has paged away from
the newest rows, and a banner above the table offers them instead ("4 new
statements", with a button to show them). Polling itself keeps running while
paused: without the requests there is nothing to count, and the banner could
never say what arrived.

The count is the difference between the API's `total` and the total the visible
table was drawn from, so it stays correct under any filter and on any page. An
explicit action, a filter or a page or a tab, always renders at once, since
that is the operator asking for new output. Showing held rows while on a later
page returns to the first page, because that is where the new rows are.

A hidden tab stops polling and asks once when it comes back, which these same
rules may turn into a banner rather than a redraw.

## No inline script or style

The edge serves a strict `Content-Security-Policy: default-src 'self'`, so an
inline `<script>`, an inline `<style>` or a `style="..."` attribute is blocked
by the browser. Styles belong in `styles.css` and behaviour in the modules
above; use a class rather than a style attribute.

`make csp-check` greps this folder for all three and fails if it finds any, so
the mistake is caught before a browser silently drops the rule. It also runs as
a prerequisite of `make edge-check`, and in CI.

For the same reason no web font is loaded from a CDN: an external origin would
need a CSP exception. Inter and JetBrains Mono are instead self-hosted from
`fonts/` and served same-origin, which `default-src 'self'` already covers. Both
are variable fonts, so one file per family carries every weight the design uses,
and both are SIL Open Font License (the licence text sits beside them).

## Look and feel

Colours, spacing, radii and the two font stacks are declared once as custom
properties on `:root` at the top of `styles.css`. Use those rather than literal
hex values, so a change to the palette reaches the whole page. The design
follows the back-office redesign mockup attached to issue #84.

## Running it

No build step. `make up` starts the stack, including the edge that serves this folder and
forwards `/api/*` to the API, and `make ui` opens https://localhost. The page and the API share
one origin, so there is no CORS and no API address to configure. The certificate is
self-signed locally, so the browser warns once.

This folder is baked into the edge image rather than mounted, so a change here needs
`make up` to rebuild before it is served; restarting the edge alone keeps the old copy.

For offline UI work against the bundled mock data (`USE_LIVE_API = false` in `api.js`), serve
this folder with `python3 -m http.server 8000` from `web/` and open http://localhost:8000.
That serves the folder directly, so edits appear on reload with no rebuild.
(Opening `index.html` directly via `file://` won't work: browsers block ES modules and `fetch`
for local files.)

## Authentication

By default (`USE_LIVE_API = true`) the page shows a login screen and
authenticates against the API's `POST /api/login`. The API answers with an
`HttpOnly`, `Secure`, `SameSite=Strict` session cookie, which script cannot read
and the browser sends by itself; the page keeps only a non-secret "signed in"
flag so a reload shows the app. State-changing requests (POST/PUT/DELETE) also
send `X-Requested-With: XMLHttpRequest`, which the API requires as its CSRF
check. You cannot view the audit log or manage servers without signing in;
logging out (`POST /api/logout`) deletes the session on the server, clears the
cookie and returns to the login screen. A 401 from any request returns to login.

The sign-in screen tells three outcomes apart, because they need different
things from the operator:

- a rejected username or password, which means try again
- `429`, the admin-login rate limit, which means wait rather than retry
- an expired session, shown when a 401 ends a session that was already signed in

A 401 also clears the local flag and stops every poller. Without that, the
background requests keep failing and each failure resets the login form while it
is being filled in.

The signed-in admin's username is stored alongside that flag, so the Admins
screen can mark your own row and leave off the actions that would end your own
session, and so the header can show who is signed in.

**Local-dev login:** against the compose stack from `make up`, sign in as `admin` /
`admin123`. That is the public development credential from `deploy/.env.example` —
use it on your own machine only. To set a different password, generate a bcrypt hash with
`htpasswd -nbBC 10 "" 'the-new-password' | cut -d: -f2` and put it in `API_ADMIN_PASS_HASH`
in `deploy/.env` (see the secrets table in [`deploy/README.md`](../deploy/README.md)).
That seeds the first admin only; after that the `admins` table is the source of truth and
accounts are managed from the Admins screen.

## Data source

Two constants near the top of `api.js` control the source:

- `USE_LIVE_API = true` (default): reads the back-office API at `API_BASE`, which is empty,
  meaning the same origin the page came from (the edge forwards `/api/*`), with login
  enforced. Requires the stack (`make up`). The Servers and Admins screens are live-only and
  hidden in mock mode.
- `USE_LIVE_API = false`:  standalone mock mode: reads the bundled mock files
  and skips the login gate, so the page works with no backend. Useful for
  offline UI work. The mock data includes a failed statement and a rejected
  login so the failure display is visible without the API.

## Files

- `index.html` - markup; loads `styles.css` and `main.js` (as an ES module)
- `styles.css` - all styling, the design tokens and the `@font-face` rules; the
  CSP forbids inline style, so none of it lives in the page itself
- `main.js` - bootstrap: wires static controls (login form, nav, filter,
  live toggle, header pill) and starts the app
- `store.js` - audit data state, the poll loop, and the filter/pagination
   query sent to the API; also decides when new rows are held back rather than
   rendered
- `api.js` - API endpoints, the signed-in flag and username storage,
  authenticated fetch, login, logout
- `format.js` - presentation helpers (`cell`, `badge`, `formatTime`,
  `badgeClass`) - stateless, safe to import anywhere
- `configstate.js` - works out whether the proxy is running the saved
  configuration; pure, so the header pill and the Servers banner agree and it
  can be tested without a browser
- `urlstate.js` - encodes the filters and open tab to a query string and back;
  pure, so the link format is tested without a browser
- `stmtkind.js` - groups a statement's keyword into a risk tier
- `actionlog.js` - turns a row of the attribution log into a sentence and says
  whether it is a server or an admin change
- `configstate.test.mjs`, `urlstate.test.mjs`, `stmtkind.test.mjs`,
  `format.test.mjs` - the pure helpers' tests, run by `make web-test`
- `views/connections.js`, `views/statements.js`, `views/servers.js`,
  `views/admins.js` - render each screen's table/form; `views/servers.js` also
  owns server create, edit and delete, and `views/admins.js` owns admin create,
  disable and password changes
- `views/pager.js` - the prev/next pager shared by both audit tables
- `views/dialog.js` - the confirm and single-field dialogs, built on the native
  `<dialog>` so Esc, the backdrop and focus are the browser's job
- `views/sessiondrawer.js` - one session and everything that ran in it, in a
  slide-over; the only view that fetches on its own
- `acumen-logo.svg` - the Acumen wordmark used in the header and on sign-in
- `fonts/` - the two self-hosted variable fonts and their licences
- `mock-connections.json` - sample connections in the API's documented shape
  (includes a rejected login)
- `mock-statements.json` - sample statements in the API's documented shape
  (covers all four kind tiers, two errors and a null schema)