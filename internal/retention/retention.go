// Package retention expires old audit records: it deletes them, or archives
// them to files and then deletes them, once they are older than a configured
// period. See docs/design/decisions/0006-audit-log-retention.md for the policy
// and why the defaults are what they are.
package retention

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/store"
)

// Action is what happens to a record when it expires.
type Action string

const (
	// ActionDelete removes expired records for good.
	ActionDelete Action = "delete"
	// ActionArchive writes expired records to a compressed file, makes sure
	// that file is on disk, and only then removes them from the database.
	ActionArchive Action = "archive"
)

/*
 * The defaults, used when nobody has set anything. Retention is OFF by default:
 * deleting audit data is irreversible, so it only happens when somebody has
 * chosen it. RecommendedDays is what we suggest they choose, and what the
 * documentation tells them to set; it is never applied on its own. The
 * reasoning is in ADR 0006; change both together.
 */
const (
	DefaultDays     = 0
	RecommendedDays = 365
	DefaultAction   = ActionDelete
	DefaultInterval = time.Hour

	// MinInterval stops a typo turning the purge into a busy loop.
	MinInterval = time.Minute

	// DefaultChunkSize is how many rows one pass reads, archives and deletes.
	// Each pass is its own short database transaction, so a large backlog
	// never holds the write lock long enough to stall audit ingest (which
	// waits only 5 seconds for it).
	DefaultChunkSize = 10_000
)

// Config is the retention policy.
type Config struct {
	// Days is how long a record is kept. Zero disables retention: nothing is
	// ever expired.
	Days int
	// Action is what happens at expiry. Ignored when Days is zero.
	Action Action
	// ArchiveDir is where archive files go. Required for ActionArchive.
	ArchiveDir string
	// Interval is how often the purge runs.
	Interval time.Duration
	// DryRun makes every run only count and log what it would remove. Nothing
	// is deleted and no archive file is written.
	DryRun bool
}

// Enabled reports whether any record will ever expire.
func (c Config) Enabled() bool { return c.Days > 0 }

/*
 * ConfigFromEnv reads the policy from the environment, using get so a test
 * need not touch the real one. Anything unrecognised is an error rather than
 * a silent default: these settings decide whether audit data is destroyed.
 *
 *	API_RETENTION_DAYS      whole days to keep records; 0 keeps them for ever (default 0)
 *	API_RETENTION_ACTION    delete or archive (default delete)
 *	API_RETENTION_ARCHIVE_DIR  directory for archive files; required for archive
 *	API_RETENTION_INTERVAL  how often to run, a Go duration (default 1h, minimum 1m)
 *	API_RETENTION_DRY_RUN   true to only log what would be removed (default false)
 */
func ConfigFromEnv(get func(string) string) (Config, error) {
	cfg := Config{Days: DefaultDays, Action: DefaultAction, Interval: DefaultInterval}

	if v := strings.TrimSpace(get("API_RETENTION_DAYS")); v != "" {
		days, err := strconv.Atoi(v)
		if err != nil || days < 0 {
			return Config{}, fmt.Errorf("API_RETENTION_DAYS=%q: must be a whole number of days, 0 or more (0 keeps records for ever)", v)
		}
		cfg.Days = days
	}

	if v := strings.TrimSpace(get("API_RETENTION_ACTION")); v != "" {
		switch Action(strings.ToLower(v)) {
		case ActionDelete:
			cfg.Action = ActionDelete
		case ActionArchive:
			cfg.Action = ActionArchive
		default:
			return Config{}, fmt.Errorf("API_RETENTION_ACTION=%q: must be %q or %q", v, ActionDelete, ActionArchive)
		}
	}

	cfg.ArchiveDir = strings.TrimSpace(get("API_RETENTION_ARCHIVE_DIR"))

	if v := strings.TrimSpace(get("API_RETENTION_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("API_RETENTION_INTERVAL=%q: not a duration such as 30m or 6h", v)
		}
		if d < MinInterval {
			return Config{}, fmt.Errorf("API_RETENTION_INTERVAL=%q: must be at least %s", v, MinInterval)
		}
		cfg.Interval = d
	}

	if v := strings.TrimSpace(get("API_RETENTION_DRY_RUN")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("API_RETENTION_DRY_RUN=%q: must be true or false", v)
		}
		cfg.DryRun = b
	}

	if cfg.Enabled() && cfg.Action == ActionArchive && cfg.ArchiveDir == "" {
		return Config{}, errors.New("API_RETENTION_ACTION=archive needs API_RETENTION_ARCHIVE_DIR")
	}
	return cfg, nil
}

/*
 * PrepareArchiveDir creates the archive directory if need be and proves a file
 * can be written there, so a wrong path or a read-only volume stops the API at
 * startup instead of on the first purge, a year from now.
 */
func PrepareArchiveDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("archive directory %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return fmt.Errorf("archive directory %s is not writable: %w", dir, err)
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// Result is what one run did.
type Result struct {
	// Cutoff is the instant records older than which expired.
	Cutoff time.Time
	// Statements and Connections are how many rows were removed, or, for a dry
	// run, how many would have been.
	Statements  int64
	Connections int64
	// Files are the archive files written, in order.
	Files []string
	// DryRun is true when nothing was changed.
	DryRun bool
}

// Runner applies a Config to a Store.
type Runner struct {
	Store *store.Store
	Cfg   Config
	// Now is the clock; nil means time.Now. Tests set it.
	Now func() time.Time
	// Logf receives one line per run and per problem; nil discards.
	Logf func(format string, args ...any)
	// ChunkSize is rows per pass; zero means DefaultChunkSize.
	ChunkSize int
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runner) chunk() int {
	if r.ChunkSize > 0 {
		return r.ChunkSize
	}
	return DefaultChunkSize
}

/*
 * Loop reports the policy and then runs the purge every Interval until ctx is
 * done. It does NOT purge on the way in: the first run is one Interval after
 * Loop starts, so restarting the API (a deploy, a crash loop, a mistake in the
 * configuration) never deletes anything by itself. It returns when ctx is done
 * and any pass in progress has finished. A failed run is logged and tried again
 * at the next tick; it never stops the loop, because the next run picks up
 * exactly where the failed one left off.
 */
func (r *Runner) Loop(ctx context.Context) {
	if !r.Cfg.Enabled() {
		return
	}
	r.reportStartup()
	t := time.NewTicker(r.Cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.runAndLog(ctx)
		}
	}
}

/*
 * reportStartup logs how much is already past the cutoff and when the first run
 * will happen, so the operator can see what the policy is about to do before it
 * does it. It only counts. A failure to count is logged and does not stop the
 * loop.
 */
func (r *Runner) reportStartup() {
	cutoff := r.now().UTC().AddDate(0, 0, -r.Cfg.Days)
	next := r.now().UTC().Add(r.Cfg.Interval)
	stmts, err := r.Store.CountExpiredStatements(cutoff)
	if err != nil {
		r.logf("retention: could not count the records past the cutoff: %v; the first run is at %s", err, next.Format(time.RFC3339))
		return
	}
	conns, err := r.Store.CountExpiredConnections(cutoff)
	if err != nil {
		r.logf("retention: could not count the records past the cutoff: %v; the first run is at %s", err, next.Format(time.RFC3339))
		return
	}
	what, note := "will be "+r.pastTense(), ""
	if r.Cfg.DryRun {
		what, note = "would be "+r.pastTense(), " (dry run: it will only be logged)"
	}
	r.logf("retention: %d statement(s) and %d connection(s) are older than %s and %s at the first run, %s%s. Nothing is removed at startup.",
		stmts, conns, cutoff.Format(time.RFC3339), what, next.Format(time.RFC3339), note)
}

func (r *Runner) runAndLog(ctx context.Context) {
	res, err := r.RunOnce(ctx)
	switch {
	case err != nil:
		r.logf("retention: run failed after removing %d statement(s) and %d connection(s) older than %s: %v",
			res.Statements, res.Connections, res.Cutoff.Format(time.RFC3339), err)
	case r.Cfg.DryRun:
		r.logf("retention: dry run: would have %s %d statement(s) and %d connection(s) older than %s; nothing was changed",
			r.pastTense(), res.Statements, res.Connections, res.Cutoff.Format(time.RFC3339))
	case res.Statements > 0 || res.Connections > 0:
		r.logf("retention: %s %d statement(s) and %d connection(s) older than %s%s",
			r.pastTense(), res.Statements, res.Connections, res.Cutoff.Format(time.RFC3339), r.fileNote(res))
	}
}

func (r *Runner) pastTense() string {
	if r.Cfg.Action == ActionArchive {
		return "archived and deleted"
	}
	return "deleted"
}

func (r *Runner) fileNote(res Result) string {
	if len(res.Files) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d archive file(s) in %s)", len(res.Files), r.Cfg.ArchiveDir)
}

/*
 * RunOnce expires everything older than the retention period, in chunks. In a
 * dry run it only counts what it would expire. For
 * ActionArchive each chunk's archive file is complete and on disk before the
 * rows in it are deleted, so a record is never gone from the database without
 * being in a file. If the process dies between the two steps, the rows are
 * simply archived again on the next run: a record may appear in two archive
 * files, never in none.
 *
 * A cancelled ctx stops it between chunks.
 */
func (r *Runner) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	if !r.Cfg.Enabled() {
		return res, nil
	}
	start := r.now().UTC()
	res.Cutoff = start.AddDate(0, 0, -r.Cfg.Days)
	if r.Cfg.DryRun {
		return r.dryRun(res)
	}
	stamp := start.Format("20060102T150405Z")
	seq := 0

	// Statements first: connections cannot go while they still have any.
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rows, err := r.Store.ExpiredStatements(res.Cutoff, r.chunk())
		if err != nil {
			return res, fmt.Errorf("find expired statements: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		if r.Cfg.Action == ActionArchive {
			seq++
			name, err := r.archive(stamp, "statements", seq, "statement", len(rows), func(emit func(any) error) error {
				for _, row := range rows {
					if err := emit(row); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return res, err
			}
			res.Files = append(res.Files, name)
		}
		ids := make([]int64, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
		}
		n, err := r.Store.DeleteStatements(ids)
		if err != nil {
			return res, fmt.Errorf("delete expired statements: %w", err)
		}
		res.Statements += n
		if n == 0 {
			return res, errors.New("deleted no statements although some were expired; stopping rather than loop")
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rows, err := r.Store.ExpiredConnections(res.Cutoff, r.chunk())
		if err != nil {
			return res, fmt.Errorf("find expired connections: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		if r.Cfg.Action == ActionArchive {
			seq++
			name, err := r.archive(stamp, "connections", seq, "connection", len(rows), func(emit func(any) error) error {
				for _, row := range rows {
					if err := emit(row); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return res, err
			}
			res.Files = append(res.Files, name)
		}
		ids := make([]string, len(rows))
		for i, row := range rows {
			ids[i] = row.ConnID
		}
		n, err := r.Store.DeleteExpiredConnections(res.Cutoff, ids)
		if err != nil {
			return res, fmt.Errorf("delete expired connections: %w", err)
		}
		res.Connections += n
		if n == 0 {
			// Every one of them was refreshed by a late event between the read
			// and the delete. Reading again would find the same rows only if
			// they were still expired, so stop for this run; the next one
			// starts clean.
			break
		}
	}
	return res, nil
}

/*
 * dryRun counts what a real run would remove and changes nothing: no rows are
 * deleted and no archive file is written, whatever the action is.
 */
func (r *Runner) dryRun(res Result) (Result, error) {
	res.DryRun = true
	var err error
	if res.Statements, err = r.Store.CountExpiredStatements(res.Cutoff); err != nil {
		return res, fmt.Errorf("count expired statements: %w", err)
	}
	if res.Connections, err = r.Store.CountExpiredConnections(res.Cutoff); err != nil {
		return res, fmt.Errorf("count expired connections: %w", err)
	}
	return res, nil
}

// archiveLine is one line of an archive file.
type archiveLine struct {
	Kind       string `json:"kind"`
	ArchivedAt string `json:"archived_at"`
	Row        any    `json:"row"`
}

/*
 * archive writes one complete archive file: gzip-compressed JSON Lines, one
 * record per line. It is written under a temporary name, flushed to disk, and
 * only then renamed into place, so a file with the final name is always whole.
 * The directory is flushed too, so the rename itself survives a crash.
 */
func (r *Runner) archive(stamp, plural string, seq int, kind string, count int, each func(emit func(any) error) error) (string, error) {
	final := filepath.Join(r.Cfg.ArchiveDir, fmt.Sprintf("audit-archive-%s-%s-%04d.jsonl.gz", stamp, plural, seq))
	if _, err := os.Stat(final); err == nil {
		return "", fmt.Errorf("archive file %s already exists; refusing to overwrite it", final)
	}
	tmp := final + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create archive file: %w", err)
	}
	cleanup := func() { f.Close(); os.Remove(tmp) }

	archivedAt := r.now().UTC().Format(time.RFC3339)
	zw := gzip.NewWriter(f)
	enc := json.NewEncoder(zw)
	written := 0
	err = each(func(row any) error {
		written++
		return enc.Encode(archiveLine{Kind: kind, ArchivedAt: archivedAt, Row: row})
	})
	if err == nil && written != count {
		err = fmt.Errorf("wrote %d of %d records", written, count)
	}
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		cleanup()
		return "", fmt.Errorf("write archive file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("close archive file: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("move archive file into place: %w", err)
	}
	if d, err := os.Open(r.Cfg.ArchiveDir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return final, nil
}
