package promptarchive

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-proxy/proxy/pkg/models"
)

func TestPromptArchiveInsertQueryDetailDelete(t *testing.T) {
	store, err := Open(":memory:", Options{Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	record := BuildRecord("req-1", []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}],"api_key":"secret"}`), models.ClaudeRequest{Model: "gpt-test", Messages: []models.ClaudeMessage{{Role: "user", Content: "hello"}}}, time.Unix(100, 0))
	if err := store.Insert(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	records, err := store.Query(context.Background(), Query{Keyword: "gpt", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RequestID != "req-1" || records[0].MessageCount != 1 {
		t.Fatalf("unexpected records: %#v", records)
	}
	detail, err := store.Detail(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Payload) == 0 || detail.Model != "gpt-test" || strings.Contains(string(detail.Payload), "secret") {
		t.Fatalf("unexpected detail: %#v", detail)
	}
	deleted, err := store.Delete(context.Background(), "req-1")
	if err != nil || !deleted {
		t.Fatalf("Delete() deleted=%v err=%v", deleted, err)
	}
}

func TestPromptArchiveSessionDeltaReconstructsAndDoesNotExposeSession(t *testing.T) {
	store, err := Open(":memory:", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := []byte(`{"model":"test","system":"` + strings.Repeat("shared context ", 400) + `A"}`)
	next := []byte(`{"model":"test","system":"` + strings.Repeat("shared context ", 400) + `B"}`)
	req := models.ClaudeRequest{Model: "test"}
	if err := store.Insert(context.Background(), BuildRecord("first", base, req, time.Unix(1, 0), "private-session")); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(context.Background(), BuildRecord("second", next, req, time.Unix(2, 0), "private-session")); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	if string(detail.Payload) != string(next) || !detail.Incremental {
		t.Fatalf("expected reconstructible delta, got %#v", detail)
	}
	if strings.Contains(string(detail.Payload), "private-session") {
		t.Fatal("session identifier was retained in payload")
	}
	stats, err := store.Statistics(context.Background())
	if err != nil || stats.DeltaCount != 1 || stats.StoredBytes >= stats.ReconstructedBytes {
		t.Fatalf("unexpected stats %#v err=%v", stats, err)
	}
}

func TestPromptArchiveDeleteMaterializesDeltaDependentRecord(t *testing.T) {
	store, err := Open(":memory:", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := []byte(`{"message":"prefix"}`)
	second := []byte(`{"message":"prefix with enough additional text to produce a compressed delta"}`)
	req := models.ClaudeRequest{Model: "test"}
	if err := store.Insert(context.Background(), BuildRecord("first", first, req, time.Unix(1, 0), "session")); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(context.Background(), BuildRecord("second", second, req, time.Unix(2, 0), "session")); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.Delete(context.Background(), "first"); err != nil || !deleted {
		t.Fatalf("delete=%v err=%v", deleted, err)
	}
	detail, err := store.Detail(context.Background(), "second")
	if err != nil || string(detail.Payload) != string(second) || detail.Incremental {
		t.Fatalf("dependent was not materialized: %#v err=%v", detail, err)
	}
}

func TestPromptArchiveLegacySchemaMigratesAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompts.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE prompt_archive (
			request_id TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			model TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			tool_count INTEGER NOT NULL DEFAULT 0,
			system_bytes INTEGER NOT NULL DEFAULT 0,
			raw_bytes INTEGER NOT NULL DEFAULT 0,
			summary TEXT NOT NULL DEFAULT '',
			truncated INTEGER NOT NULL DEFAULT 0,
			payload_hash TEXT NOT NULL DEFAULT ''
		)`)
	if err == nil {
		_, err = legacy.Exec(`
			CREATE TABLE prompt_archive_payloads (
				request_id TEXT PRIMARY KEY,
				created_at INTEGER NOT NULL,
				payload BLOB NOT NULL,
				FOREIGN KEY (request_id) REFERENCES prompt_archive(request_id) ON DELETE CASCADE
			)`)
	}
	if err == nil {
		_, err = legacy.Exec(`INSERT INTO prompt_archive(request_id, created_at, model, payload_hash) VALUES (?, ?, ?, ?)`, "legacy", 1000, "old-model", hashPayload([]byte(`{"legacy":true}`)))
	}
	if err == nil {
		_, err = legacy.Exec(`INSERT INTO prompt_archive_payloads(request_id, created_at, payload) VALUES (?, ?, ?)`, "legacy", 1000, []byte(`{"legacy":true}`))
	}
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path, Options{Now: func() time.Time { return time.Unix(2, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(context.Background(), "legacy")
	if err != nil || string(detail.Payload) != `{"legacy":true}` || detail.StorageKind != "full" || detail.StoredBytes != len(detail.Payload) || detail.ReconstructedBytes != len(detail.Payload) {
		t.Fatalf("legacy record was not migrated correctly: %#v err=%v", detail, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path, Options{Now: func() time.Time { return time.Unix(2, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	if detail, err = store.Detail(context.Background(), "legacy"); err != nil || string(detail.Payload) != `{"legacy":true}` {
		_ = store.Close()
		t.Fatalf("migrated record did not survive reopen: %#v err=%v", detail, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPromptArchiveRetentionUsesInjectedClock(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "prompts.db")
	store, err := Open(path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []Record{
		{RequestID: "expired", CreatedAt: now.Add(-2 * time.Hour), Payload: []byte(`{"expired":true}`)},
		{RequestID: "boundary", CreatedAt: now.Add(-time.Hour), Payload: []byte(`{"boundary":true}`)},
	} {
		if err := store.Insert(context.Background(), record); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path, Options{Retention: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records, err := store.Query(context.Background(), Query{Limit: 10})
	if err != nil || len(records) != 1 || records[0].RequestID != "boundary" {
		t.Fatalf("retention did not keep only the boundary record: %#v err=%v", records, err)
	}
	if _, err := store.Detail(context.Background(), "expired"); err == nil {
		t.Fatal("expired payload remained accessible after retention cleanup")
	}
}

func TestPromptArchiveDeleteMaterializesMultiLevelDependents(t *testing.T) {
	store, err := Open(":memory:", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	contextPrefix := strings.Repeat("shared context ", 400)
	payloads := map[string][]byte{
		"first":  []byte(`{"system":"` + contextPrefix + `A"}`),
		"second": []byte(`{"system":"` + contextPrefix + `B"}`),
		"third":  []byte(`{"system":"` + contextPrefix + `C"}`),
	}
	for i, id := range []string{"first", "second", "third"} {
		if err := store.Insert(context.Background(), BuildRecord(id, payloads[id], models.ClaudeRequest{Model: "test"}, time.Unix(int64(i+1), 0), "session")); err != nil {
			t.Fatal(err)
		}
	}
	var secondBase, thirdBase string
	if err := store.db.QueryRow(`SELECT base_request_id FROM prompt_archive WHERE request_id = 'second'`).Scan(&secondBase); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT base_request_id FROM prompt_archive WHERE request_id = 'third'`).Scan(&thirdBase); err != nil {
		t.Fatal(err)
	}
	if secondBase != "first" || thirdBase != "second" {
		t.Fatalf("expected a multi-level delta chain, got second=%q third=%q", secondBase, thirdBase)
	}
	if deleted, err := store.Delete(context.Background(), "first"); err != nil || !deleted {
		t.Fatalf("delete=%v err=%v", deleted, err)
	}
	for _, id := range []string{"second", "third"} {
		detail, err := store.Detail(context.Background(), id)
		if err != nil || detail.Incremental || detail.StorageKind != "full" || string(detail.Payload) != string(payloads[id]) {
			t.Fatalf("dependent %q was not materialized: %#v err=%v", id, detail, err)
		}
	}
}

func TestPromptArchiveExportMarksMissingPayloadUnavailable(t *testing.T) {
	store, err := Open(":memory:", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Insert(context.Background(), Record{RequestID: "missing", CreatedAt: time.Unix(1, 0), Payload: []byte(`{"value":true}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM prompt_archive_payloads WHERE request_id = 'missing'`); err != nil {
		t.Fatal(err)
	}

	records, err := store.Export(context.Background(), Query{}, 1000)
	if err != nil || len(records) != 1 || records[0].PayloadState != "unavailable" || len(records[0].Payload) != 0 {
		t.Fatalf("export did not expose unavailable payload state: %#v err=%v", records, err)
	}
}

func TestPromptArchiveExportReconstructsBulkWithSharedCache(t *testing.T) {
	store, err := Open(":memory:", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := []byte(`{"model":"test","system":"` + strings.Repeat("shared context ", 400) + `A"}`)
	req := models.ClaudeRequest{Model: "test"}
	if err := store.Insert(context.Background(), BuildRecord("base", base, req, time.Unix(1, 0), "session")); err != nil {
		t.Fatal(err)
	}
	for i, suffix := range []string{"B", "C"} {
		payload := []byte(`{"model":"test","system":"` + strings.Repeat("shared context ", 400) + suffix + `"}`)
		if err := store.Insert(context.Background(), BuildRecord("delta-"+suffix, payload, req, time.Unix(int64(i+2), 0), "session")); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.Export(context.Background(), Query{Limit: 10}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records", len(records))
	}
	for _, record := range records {
		if len(record.Payload) == 0 || record.PayloadState == "unavailable" {
			t.Fatalf("record was not reconstructed: %#v", record)
		}
	}
}
