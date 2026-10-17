package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestTLSMigrationPreservesExistingServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE servers (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, listen_port INTEGER UNIQUE NOT NULL, upstream_host TEXT NOT NULL, upstream_port INTEGER NOT NULL, upstream_user TEXT NOT NULL, upstream_pass TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1);
 INSERT INTO servers VALUES (1,'old',3307,'db-a',3306,'bootstrap','encrypted-secret',1)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < 2; i++ {
		s, err := InitDB(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := s.GetServers()
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].TLS != "verify" || rows[0].TLSCAFile != "" || rows[0].UpstreamPass != "encrypted-secret" {
			t.Fatal("migration lost data or secure defaults")
		}
		s.Close()
	}
}
