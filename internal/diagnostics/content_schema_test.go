package diagnostics

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenRejectsUnexpectedDiagnosticsTrigger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	store, err := Open(path, StoreOptions{CaptureContent: true})
	if err != nil { t.Fatal(err) }
	if err := store.Close(); err != nil { t.Fatal(err) }
	db, err := sql.Open("sqlite", path)
	if err != nil { t.Fatal(err) }
	_, err = db.Exec(`CREATE TRIGGER unexpected_diagnostics_trigger AFTER INSERT ON diagnostics_events BEGIN SELECT 1; END;`)
	if closeErr := db.Close(); closeErr != nil { t.Fatal(closeErr) }
	if err != nil { t.Fatal(err) }
	if _, err := Open(path, StoreOptions{CaptureContent: true}); err == nil || !strings.Contains(err.Error(), "unexpected trigger") {
		t.Fatalf("Open() error = %v, want unexpected trigger rejection", err)
	}
}

func TestOpenRejectsIncompatibleDiagnosticsContentForeignKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	db, err := sql.Open("sqlite", path)
	if err != nil { t.Fatal(err) }
	_, err = db.Exec(`CREATE TABLE diagnostics_events (request_id TEXT PRIMARY KEY); CREATE TABLE diagnostics_content (request_id TEXT NOT NULL, attempt_number INTEGER NOT NULL, boundary TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, capture_mode TEXT NOT NULL, content BLOB NOT NULL, PRIMARY KEY(request_id, attempt_number, boundary), FOREIGN KEY(request_id) REFERENCES diagnostics_events(request_id)); PRAGMA user_version = 7;`)
	if closeErr := db.Close(); closeErr != nil { t.Fatal(closeErr) }
	if err != nil { t.Fatal(err) }
	if _, err := Open(path, StoreOptions{CaptureContent: true}); err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("Open() error = %v, want content foreign key rejection", err)
	}
}
