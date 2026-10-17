package retention

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lustre/internal/audit"
	"lustre/internal/store"
)

// now is the clock for every test. With 30 days of retention the cutoff is
// 2026-09-15, so "old" records expire and "recent" ones do not.
var now = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

const (
	old    = "2026-08-01T10:00:00Z"
	recent = "2026-10-01T10:00:00Z"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.InitDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func write(t *testing.T, s *store.Store, events ...audit.Event) {
	t.Helper()
	if err := s.WriteBatch(events); err != nil {
		t.Fatal(err)
	}
}

// seed stores: an old finished session with two statements, an old failed
// login, and a recent session with one statement.
func seed(t *testing.T, s *store.Store) {
	t.Helper()
	write(t, s,
		audit.Event{ConnID: "old", Kind: audit.KindConnect, At: old, User: "alice", Upstream: "db-a", AuthResult: audit.AuthOK},
		audit.Event{ConnID: "old", Kind: audit.KindStatement, At: old, SQL: "SELECT 'old1'", StmtType: "SELECT"},
		audit.Event{ConnID: "old", Kind: audit.KindStatement, At: "2026-08-01T10:00:01Z", SQL: "SELECT 'old2'", StmtType: "SELECT"},
		audit.Event{ConnID: "old", Kind: audit.KindDisconnect, At: "2026-08-01T10:05:00Z"},
		audit.Event{ConnID: "failed", Kind: audit.KindAuthFail, At: old, User: "mallory", AuthResult: audit.AuthFail, Err: "ERROR 1045: Access denied"},
		audit.Event{ConnID: "new", Kind: audit.KindConnect, At: recent, User: "bob", Upstream: "db-a", AuthResult: audit.AuthOK},
		audit.Event{ConnID: "new", Kind: audit.KindStatement, At: recent, SQL: "SELECT 'new'", StmtType: "SELECT"},
	)
}

func remaining(t *testing.T, s *store.Store) (conns []string, stmts []string) {
	t.Helper()
	cs, _, err := s.GetConnections(store.Filter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		conns = append(conns, c.ConnID)
	}
	ss, _, err := s.GetStatements(store.StatementFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range ss {
		stmts = append(stmts, st.SQL)
	}
	return conns, stmts
}

func runner(s *store.Store, cfg Config) *Runner {
	return &Runner{Store: s, Cfg: cfg, Now: func() time.Time { return now }}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := ConfigFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Days != 0 || cfg.Enabled() || cfg.Action != ActionDelete || cfg.Interval != time.Hour || cfg.DryRun {
		t.Fatalf("defaults = %+v, want retention off (0 days), delete, hourly, no dry run", cfg)
	}
}

// 365 is what we recommend, never what happens by itself.
func TestRecommendedIsNotTheDefault(t *testing.T) {
	if RecommendedDays != 365 {
		t.Fatalf("RecommendedDays = %d, want 365", RecommendedDays)
	}
	if DefaultDays != 0 {
		t.Fatalf("DefaultDays = %d: deleting audit data must be opt-in", DefaultDays)
	}
	cfg, err := ConfigFromEnv(env(map[string]string{"API_RETENTION_ACTION": "delete"}))
	if err != nil || cfg.Enabled() {
		t.Fatalf("choosing an action alone must not switch retention on: %+v, %v", cfg, err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(Config) bool
	}{
		{"30 days", map[string]string{"API_RETENTION_DAYS": "30"}, "", func(c Config) bool { return c.Days == 30 }},
		{"zero disables", map[string]string{"API_RETENTION_DAYS": "0"}, "", func(c Config) bool { return !c.Enabled() }},
		{"spaces are trimmed", map[string]string{"API_RETENTION_DAYS": " 90 "}, "", func(c Config) bool { return c.Days == 90 }},
		{"archive with a dir", map[string]string{"API_RETENTION_DAYS": "30", "API_RETENTION_ACTION": "archive", "API_RETENTION_ARCHIVE_DIR": "/a"}, "", func(c Config) bool { return c.Action == ActionArchive && c.ArchiveDir == "/a" }},
		{"action is case-insensitive", map[string]string{"API_RETENTION_ACTION": "DELETE"}, "", func(c Config) bool { return c.Action == ActionDelete }},
		{"interval", map[string]string{"API_RETENTION_INTERVAL": "6h"}, "", func(c Config) bool { return c.Interval == 6*time.Hour }},
		{"dry run on", map[string]string{"API_RETENTION_DRY_RUN": "true"}, "", func(c Config) bool { return c.DryRun }},
		{"dry run off", map[string]string{"API_RETENTION_DRY_RUN": "false"}, "", func(c Config) bool { return !c.DryRun }},
		{"dry run is case-insensitive and trimmed", map[string]string{"API_RETENTION_DRY_RUN": " TRUE "}, "", func(c Config) bool { return c.DryRun }},
		{"dry run not a boolean", map[string]string{"API_RETENTION_DRY_RUN": "maybe"}, "API_RETENTION_DRY_RUN", nil},
		{"negative days", map[string]string{"API_RETENTION_DAYS": "-1"}, "API_RETENTION_DAYS", nil},
		{"days not a number", map[string]string{"API_RETENTION_DAYS": "a year"}, "API_RETENTION_DAYS", nil},
		{"fractional days", map[string]string{"API_RETENTION_DAYS": "1.5"}, "API_RETENTION_DAYS", nil},
		{"unknown action", map[string]string{"API_RETENTION_ACTION": "purge"}, "API_RETENTION_ACTION", nil},
		{"archive without a dir", map[string]string{"API_RETENTION_DAYS": "30", "API_RETENTION_ACTION": "archive"}, "API_RETENTION_ARCHIVE_DIR", nil},
		{"interval not a duration", map[string]string{"API_RETENTION_INTERVAL": "daily"}, "API_RETENTION_INTERVAL", nil},
		{"interval too short", map[string]string{"API_RETENTION_INTERVAL": "5s"}, "API_RETENTION_INTERVAL", nil},
		// Retention off: an archive setting that would otherwise be incomplete is harmless.
		{"disabled needs no archive dir", map[string]string{"API_RETENTION_DAYS": "0", "API_RETENTION_ACTION": "archive"}, "", func(c Config) bool { return !c.Enabled() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := ConfigFromEnv(env(c.env))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one naming %s", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !c.check(cfg) {
				t.Fatalf("config = %+v", cfg)
			}
		})
	}
}

func TestPrepareArchiveDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "archive")
	if err := PrepareArchiveDir(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("the writability probe left %d file(s) behind", len(entries))
	}

	file := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(file, []byte("x"), 0o600)
	if err := PrepareArchiveDir(file); err == nil {
		t.Fatal("a path that is a file was accepted as an archive directory")
	}
}

func TestDeleteRemovesOnlyExpiredRecords(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	res, err := runner(s, Config{Days: 30, Action: ActionDelete}).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Statements != 2 || res.Connections != 2 {
		t.Fatalf("removed %d statements and %d connections, want 2 and 2", res.Statements, res.Connections)
	}
	if len(res.Files) != 0 {
		t.Fatalf("delete mode wrote files: %v", res.Files)
	}
	conns, stmts := remaining(t, s)
	if len(conns) != 1 || conns[0] != "new" || len(stmts) != 1 || stmts[0] != "SELECT 'new'" {
		t.Fatalf("remaining = %v / %v, want only the recent session", conns, stmts)
	}
}

func TestSecondRunHasNothingToDo(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	r := runner(s, Config{Days: 30, Action: ActionDelete})
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := r.RunOnce(context.Background())
	if err != nil || res.Statements != 0 || res.Connections != 0 {
		t.Fatalf("second run = %+v, %v; want nothing", res, err)
	}
}

func TestDisabledNeverDeletes(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	res, err := runner(s, Config{Days: 0, Action: ActionDelete}).RunOnce(context.Background())
	if err != nil || res.Statements != 0 || res.Connections != 0 {
		t.Fatalf("disabled run = %+v, %v", res, err)
	}
	conns, stmts := remaining(t, s)
	if len(conns) != 3 || len(stmts) != 3 {
		t.Fatalf("remaining = %v / %v, want everything", conns, stmts)
	}
}

// The window is measured back from now: a longer retention keeps more.
func TestLongerRetentionKeepsMore(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	// 90 days before 15 Oct is 17 Jul: the August records are inside it.
	res, err := runner(s, Config{Days: 90, Action: ActionDelete}).RunOnce(context.Background())
	if err != nil || res.Statements != 0 || res.Connections != 0 {
		t.Fatalf("90-day run = %+v, %v; want nothing expired", res, err)
	}
}

// readArchive returns every line of an archive file, decoded.
func readArchive(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s is not gzip: %v", path, err)
	}
	var out []map[string]any
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad line in %s: %v", path, err)
		}
		out = append(out, m)
	}
	return out
}

func TestArchiveWritesEverythingThenDeletes(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	dir := t.TempDir()

	res, err := runner(s, Config{Days: 30, Action: ActionArchive, ArchiveDir: dir}).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Statements != 2 || res.Connections != 2 || len(res.Files) != 2 {
		t.Fatalf("result = %+v, want 2 statements, 2 connections, 2 files", res)
	}

	var stmtSQL, connIDs []string
	for _, f := range res.Files {
		info, _ := os.Stat(f)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", f, info.Mode().Perm())
		}
		for _, line := range readArchive(t, f) {
			row := line["row"].(map[string]any)
			if line["archived_at"] != now.Format(time.RFC3339) {
				t.Errorf("archived_at = %v", line["archived_at"])
			}
			switch line["kind"] {
			case "statement":
				stmtSQL = append(stmtSQL, row["sql"].(string))
			case "connection":
				connIDs = append(connIDs, row["conn_id"].(string))
				if row["conn_id"] == "failed" && row["auth_fail_reason"] != "ERROR 1045: Access denied" {
					t.Errorf("the archived failed login lost its reason: %v", row["auth_fail_reason"])
				}
			default:
				t.Errorf("unknown kind %v", line["kind"])
			}
		}
	}
	if len(stmtSQL) != 2 || len(connIDs) != 2 {
		t.Fatalf("archived statements %v and connections %v, want 2 and 2", stmtSQL, connIDs)
	}

	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
	conns, _ := remaining(t, s)
	if len(conns) != 1 || conns[0] != "new" {
		t.Fatalf("remaining connections = %v, want only the recent one", conns)
	}
}

// The promise that matters most: if the archive cannot be written, nothing is deleted.
func TestFailedArchiveDeletesNothing(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	// A directory that does not exist, so creating the file fails.
	missing := filepath.Join(t.TempDir(), "gone")
	_, err := runner(s, Config{Days: 30, Action: ActionArchive, ArchiveDir: missing}).RunOnce(context.Background())
	if err == nil {
		t.Fatal("run succeeded although the archive could not be written")
	}
	conns, stmts := remaining(t, s)
	if len(conns) != 3 || len(stmts) != 3 {
		t.Fatalf("after a failed archive: %v / %v, want every record still there", conns, stmts)
	}
}

func TestArchiveRefusesToOverwriteAnExistingFile(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	dir := t.TempDir()
	existing := filepath.Join(dir, "audit-archive-20261015T120000Z-statements-0001.jsonl.gz")
	if err := os.WriteFile(existing, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runner(s, Config{Days: 30, Action: ActionArchive, ArchiveDir: dir}).RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want a refusal to overwrite", err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "precious" {
		t.Fatal("an existing archive file was overwritten")
	}
	if _, stmts := remaining(t, s); len(stmts) != 3 {
		t.Fatalf("statements were deleted despite the failed archive: %v", stmts)
	}
}

// A backlog is worked through in chunks, each its own complete file.
func TestChunksProduceOneCompleteFileEach(t *testing.T) {
	s := newStore(t)
	events := []audit.Event{{ConnID: "c", Kind: audit.KindConnect, At: old, User: "u", Upstream: "db-a", AuthResult: audit.AuthOK}}
	for i := 0; i < 7; i++ {
		events = append(events, audit.Event{ConnID: "c", Kind: audit.KindStatement, At: old, SQL: fmt.Sprintf("SELECT %d", i), StmtType: "SELECT"})
	}
	write(t, s, events...)
	dir := t.TempDir()

	r := runner(s, Config{Days: 30, Action: ActionArchive, ArchiveDir: dir})
	r.ChunkSize = 3
	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Statements != 7 || res.Connections != 1 {
		t.Fatalf("removed %d statements and %d connections, want 7 and 1", res.Statements, res.Connections)
	}
	// 7 statements in chunks of 3 is 3 files, plus 1 for the connection.
	if len(res.Files) != 4 {
		t.Fatalf("%d archive files, want 4: %v", len(res.Files), res.Files)
	}
	seen := map[string]bool{}
	total := 0
	for _, f := range res.Files {
		if seen[f] {
			t.Fatalf("file %s reported twice", f)
		}
		seen[f] = true
		total += len(readArchive(t, f))
	}
	if total != 8 {
		t.Fatalf("archive files hold %d records in all, want 8", total)
	}
}

func TestCancelledContextStopsBeforeDeleting(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runner(s, Config{Days: 30, Action: ActionDelete}).RunOnce(ctx)
	if err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if conns, _ := remaining(t, s); len(conns) != 3 {
		t.Fatalf("a cancelled run deleted records: %v", conns)
	}
}

// Audit ingest must keep working while a purge runs.
func TestIngestAndPurgeCanRunTogether(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	r := runner(s, Config{Days: 30, Action: ActionDelete})
	r.ChunkSize = 1

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := r.RunOnce(context.Background())
		errs <- err
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := s.WriteBatch([]audit.Event{{ConnID: "new", Kind: audit.KindStatement, At: recent, SQL: fmt.Sprintf("SELECT live %d", i), StmtType: "SELECT"}}); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	_, stmts := remaining(t, s)
	if len(stmts) != 51 {
		t.Fatalf("%d statements remain, want the 1 recent plus the 50 written meanwhile", len(stmts))
	}
}

// logCollector gathers the lines a Runner logs.
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCollector) logf(f string, a ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
	l.mu.Unlock()
}

func (l *logCollector) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// waitFor polls until some logged line contains want, or fails after a few seconds.
func (l *logCollector) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, line := range l.snapshot() {
			if strings.Contains(line, want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log line containing %q; got %q", want, l.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stopLoop(t *testing.T, cancel context.CancelFunc, done chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not return after cancel")
	}
}

// Restarting the API must never delete anything by itself: the first purge is
// one Interval after the loop starts, not at once.
func TestLoopDoesNotPurgeOnStartup(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	logs := &logCollector{}
	r := runner(s, Config{Days: 30, Action: ActionDelete, Interval: time.Hour})
	r.Logf = logs.logf

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Loop(ctx); close(done) }()

	// The startup report is the sign the loop is up, and it counts without deleting.
	logs.waitFor(t, "Nothing is removed at startup")
	time.Sleep(200 * time.Millisecond)
	stopLoop(t, cancel, done)

	conns, stmts := remaining(t, s)
	if len(conns) != 3 || len(stmts) != 3 {
		t.Fatalf("the loop deleted on startup: %v / %v remain, want everything", conns, stmts)
	}
	for _, line := range logs.snapshot() {
		if strings.HasPrefix(line, "retention: deleted ") || strings.HasPrefix(line, "retention: archived and deleted ") {
			t.Fatalf("a purge ran on startup: %q", line)
		}
	}
}

func TestLoopPurgesOnTheTickAndStopsOnCancel(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	logs := &logCollector{}
	r := runner(s, Config{Days: 30, Action: ActionDelete, Interval: 300 * time.Millisecond})
	r.Logf = logs.logf

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() { r.Loop(ctx); close(done) }()

	logs.waitFor(t, "deleted 2 statement(s) and 2 connection(s)")
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Fatalf("the first purge came after %v, before one Interval had passed", waited)
	}
	stopLoop(t, cancel, done)

	conns, _ := remaining(t, s)
	if len(conns) != 1 || conns[0] != "new" {
		t.Fatalf("remaining connections = %v, want only the recent one", conns)
	}
}

// The startup line says how much is already past the cutoff and when the first
// run is, in the same terms the run will later report.
func TestStartupReportSaysWhatIsDueAndWhen(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	logs := &logCollector{}
	r := runner(s, Config{Days: 30, Action: ActionDelete, Interval: 6 * time.Hour})
	r.Logf = logs.logf

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Loop(ctx); close(done) }()
	logs.waitFor(t, "Nothing is removed at startup")
	stopLoop(t, cancel, done)

	line := logs.snapshot()[0]
	for _, want := range []string{
		"2 statement(s) and 2 connection(s)",
		"older than 2026-09-15T12:00:00Z", // 30 days before the test clock
		"will be deleted",
		"first run, 2026-10-15T18:00:00Z", // 6 hours after the test clock
	} {
		if !strings.Contains(line, want) {
			t.Errorf("startup line %q does not contain %q", line, want)
		}
	}
	if conns, _ := remaining(t, s); len(conns) != 3 {
		t.Fatalf("reporting deleted records: %v remain", conns)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	for _, action := range []Action{ActionDelete, ActionArchive} {
		t.Run(string(action), func(t *testing.T) {
			s := newStore(t)
			seed(t, s)
			dir := t.TempDir()

			res, err := runner(s, Config{Days: 30, Action: action, ArchiveDir: dir, DryRun: true}).RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !res.DryRun || res.Statements != 2 || res.Connections != 2 {
				t.Fatalf("result = %+v, want a dry run that counts 2 statements and 2 connections", res)
			}
			if len(res.Files) != 0 {
				t.Fatalf("a dry run wrote archive files: %v", res.Files)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("a dry run left %d file(s) in the archive directory", len(entries))
			}
			conns, stmts := remaining(t, s)
			if len(conns) != 3 || len(stmts) != 3 {
				t.Fatalf("a dry run deleted records: %v / %v remain", conns, stmts)
			}
		})
	}
}

func TestDryRunLoopLogsWhatItWouldDo(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	logs := &logCollector{}
	r := runner(s, Config{Days: 30, Action: ActionDelete, Interval: 100 * time.Millisecond, DryRun: true})
	r.Logf = logs.logf

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Loop(ctx); close(done) }()
	logs.waitFor(t, "dry run: would have deleted 2 statement(s) and 2 connection(s)")
	stopLoop(t, cancel, done)

	if conns, _ := remaining(t, s); len(conns) != 3 {
		t.Fatalf("the dry-run loop deleted records: %v remain", conns)
	}
	for _, line := range logs.snapshot() {
		if strings.Contains(line, "nothing was changed") {
			return
		}
	}
	t.Fatalf("no line said nothing was changed: %q", logs.snapshot())
}

func TestLoopDoesNothingWhenDisabled(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	done := make(chan struct{})
	go func() { runner(s, Config{Days: 0}).Loop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop blocked although retention is disabled")
	}
}
