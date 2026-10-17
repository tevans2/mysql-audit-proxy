# ADR 0006: Audit-log retention is configurable and off by default; we recommend 365 days and delete

## Status

Accepted, 2026-10-15, and revised after review the same day: retention is **off by
default**, and 365 days with delete is the setting we recommend rather than one we
apply. **Provisional until Acumen answers.**
How long to keep the log, and whether to delete or archive at expiry, was
question 5 at the 24 September Sprint 2 demo and was not reached (see the
"Not covered" line in [that meeting's notes](../../meetings/2026-09-24-sprint-2-demo.md)).
The ticket asks for the period and the action to follow Acumen's answer, or our
stated default if there is none. This ADR is the stated default: no deletion
until somebody turns it on, with 365 days and delete as the recommended setting.
When Acumen answers, set the variables below to their answer (or, if they want
retention by default, change the default in `internal/retention` and this record).

## Context

The audit log grows for ever. It also stores raw SQL, literal values included,
so it holds whatever personal information appears in queries and is relevant to
POPIA, which expects personal information not to be kept longer than needed
(known-limitations.md, "Security and privacy"). Keeping it indefinitely is a
decision too, and it should be one somebody made on purpose.

Two things are the client's to decide and not ours: how long, and whether an
expired record is destroyed or kept somewhere else.

## Decision

Records older than a configured number of days expire. What happens then is
configurable. Five variables on the API, read at startup and validated there (an
invalid value stops the API, since these settings decide whether audit data is
destroyed):

| Variable | Default | Meaning |
|---|---|---|
| `API_RETENTION_DAYS` | `0` (off) | Whole days to keep records. `0` disables retention: nothing is ever expired. Recommended: `365`. |
| `API_RETENTION_ACTION` | `delete` | `delete` removes expired records. `archive` writes them to files first, then removes them. |
| `API_RETENTION_ARCHIVE_DIR` | none | Where archive files go. Required for `archive`; checked writable at startup. |
| `API_RETENTION_INTERVAL` | `1h` | How often the purge runs. At least `1m`. |
| `API_RETENTION_DRY_RUN` | `false` | `true` logs what would be removed and removes nothing: no rows are deleted and no archive file is written. |

**Off by default; recommended 365 days, delete.** Deleting audit records cannot
be undone, and nobody with the authority to decide has said how long to keep
them. So the API deletes nothing until `API_RETENTION_DAYS` is set to a positive
number, and an upgrade never destroys data by itself. We recommend 365 days with
`delete`: a year covers a normal audit and review cycle without keeping personal
data indefinitely, and archiving needs a place to put the files that we cannot
choose for the client (an archive nobody manages is the same retention problem
with a different name). The number is ours, not Acumen's, which is why it is a
recommendation in the documentation and the compose file's comment, and
`retention.RecommendedDays` in the code, but not the default.

**Nothing happens at startup.** The first purge is one interval after the API
starts, never at once, so restarting the API (a deploy, a crash loop, a
configuration mistake) cannot delete anything. At startup the API logs how many
statements and connections are already older than the cutoff and the time of the
first run, so the operator can see what the policy is about to do before it does
it:

```
retention: 4021 statement(s) and 87 connection(s) are older than 2025-10-15T12:00:00Z and will be deleted at the first run, 2026-10-15T13:00:00Z. Nothing is removed at startup.
```

**Dry run.** With `API_RETENTION_DRY_RUN=true` every run only counts and logs
(`dry run: would have deleted N statement(s) and M connection(s) …; nothing was
changed`). Nothing is deleted and no archive file is written, in either action,
so a policy can be tried on real data before it is allowed to act. A dry run
does not create or check the archive directory either, so switching it off is
the first time that directory is touched.

**What expires.** A statement expires when its own timestamp is older than the
cutoff. A connection expires when its last activity (its disconnect time, or
its connect time if it never disconnected) is older than the cutoff and it has
no statements left. So a session that is still open, or that ran something
inside the window, keeps its connection row while its older statements go. A
failed login has no statements, so it expires with its timestamp. A record with
no timestamp is never expired, because it cannot be dated.

**How archiving is made safe.** Expiry works in chunks of 10,000 rows. For each
chunk the archive file is written under a temporary name, flushed to disk,
renamed into place and the directory flushed, and only then are the rows deleted.
If anything fails before that, nothing is deleted. If the process dies after the
file is safe but before the delete commits, the same rows are archived again
next run: a record can appear in two archive files, never in none. The file name
carries the run time, `statements` or `connections`, and a sequence number, and
an existing file is never overwritten.

**Archive format.** gzip-compressed JSON Lines, one record per line:
`{"kind":"statement"|"connection","archived_at":"…","row":{…}}`, with every
column the database holds. Files are created with mode 0600.

**Chunking is what keeps ingest working.** The API's database allows a writer
five seconds of waiting for the write lock. One transaction deleting a year of
statements would hold it for longer, and the proxy's audit posts would start
failing. Each chunk is a short transaction of its own, and the test suite
writes audit events while a purge runs.

## Consequences

**Positive**

- The audit log has a stated, configurable lifetime, which was an open
  compliance question.
- Archive mode never loses a record to a failed write, and the archive is
  self-describing.
- Nothing is destroyed by an upgrade or a restart: deletion only starts when
  somebody sets the days, and a dry run shows what it would do first.
- The startup log states the policy and how much is due, so a surprise is
  visible before it happens.

**Negative, and open**

- **Delete is irreversible.** It is opt-in, but anyone who sets
  `API_RETENTION_DAYS` with `delete` will lose the records older than that on
  the first run, one interval after starting. Use `API_RETENTION_DRY_RUN=true`
  first, or `archive`.
- **Off by default means the log grows and personal data is kept** until
  somebody turns retention on. That is the situation before this feature, and
  the documentation and the startup log both say retention is off.
- **The startup counts are a snapshot.** They are taken when the API starts
  and the purge runs later, so the numbers at the first run will differ if
  records keep arriving or ageing past the cutoff in between. A connection is
  counted when its last activity and all its statements are past the cutoff.
- **A restart delays the purge.** The first run is one interval after each
  start, so an API restarted more often than the interval (a crash loop, say)
  never purges. That is the price of a restart never deleting anything.
- **Archive files hold raw SQL too.** They carry the same personal information
  as the database, unencrypted, so the archive directory needs the same access
  control and its own retention. Nothing here expires the archive files.
- **Archives are not tamper-evident**, the same as the database
  (known-limitations.md); tamper-evidence is separate work.
- **The database file does not shrink.** SQLite reuses the freed space for new
  records but does not return it to the disk. `VACUUM` does, and is left to the
  operator because it needs free space and exclusive access.
- **Each purge deletes by the server's clock.** A clock that jumps forward
  expires records early; there is no guard.
- **A late event for an expired record.** If the proxy delivers an event whose
  timestamp is already past the cutoff, it is stored and then removed by the
  next run. A statement that arrives for a connection already removed cannot
  be stored at all (the foreign key), which is the existing behaviour for a
  statement with no connection.
- **No endpoint or screen shows retention.** Each run that removes something
  logs one line; nothing is recorded in the database. The log line, and the
  archive files in archive mode, are the trail.

## Alternatives considered

- **365 days and delete as the default** (the first version of this ADR).
  Gives the ticket's "stated default" a working value, but it deletes data on
  the first start after an upgrade for anyone who did not read the release
  notes. Rejected in review in favour of off by default with a recommendation.
- **Archive as the default.** Safer against accidental loss, but it forces a
  directory choice we cannot make for the client and produces an unmanaged pile
  of personal data. Available with one setting.
- **Purging once at startup, then on the interval.** Catches up at once after a
  long outage, but every restart then deletes. Rejected: the first run is one
  interval after startup instead.
- **Deleting a whole connection with its statements by the connection's age.**
  Simpler, but it would delete the older statements of a session that is still
  running, or keep a session's statements for longer than the period.
- **One transaction per run.** Simpler, and blocks ingest on a large backlog.
- **A scheduled job outside the API** (cron, a sidecar). Needs a second process
  that opens the same SQLite file, for something that fits in the API.