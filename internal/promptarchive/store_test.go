package promptarchive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-proxy/proxy/pkg/models"
)

func TestPromptArchiveInsertQueryDetailDelete(t *testing.T) {
	store, err := Open(":memory:", Options{Retention: time.Hour})
	if err != nil { t.Fatal(err) }
	defer store.Close()

	record := BuildRecord("req-1", []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}],"api_key":"secret"}`), models.ClaudeRequest{Model: "gpt-test", Messages: []models.ClaudeMessage{{Role: "user", Content: "hello"}}}, time.Unix(100, 0))
	if err := store.Insert(context.Background(), record); err != nil { t.Fatal(err) }
	records, err := store.Query(context.Background(), Query{Keyword: "gpt", Limit: 10})
	if err != nil { t.Fatal(err) }
	if len(records) != 1 || records[0].RequestID != "req-1" || records[0].MessageCount != 1 { t.Fatalf("unexpected records: %#v", records) }
	detail, err := store.Detail(context.Background(), "req-1")
	if err != nil { t.Fatal(err) }
	if len(detail.Payload) == 0 || detail.Model != "gpt-test" || strings.Contains(string(detail.Payload), "secret") { t.Fatalf("unexpected detail: %#v", detail) }
	deleted, err := store.Delete(context.Background(), "req-1")
	if err != nil || !deleted { t.Fatalf("Delete() deleted=%v err=%v", deleted, err) }
}

func TestPromptArchiveSessionDeltaReconstructsAndDoesNotExposeSession(t *testing.T) {
	store, err := Open(":memory:", Options{})
	if err != nil { t.Fatal(err) }
	defer store.Close()
	base := []byte(`{"model":"test","system":"` + strings.Repeat("shared context ", 400) + `A"}`)
	next := []byte(`{"model":"test","system":"` + strings.Repeat("shared context ", 400) + `B"}`)
	req := models.ClaudeRequest{Model: "test"}
	if err := store.Insert(context.Background(), BuildRecord("first", base, req, time.Unix(1, 0), "private-session")); err != nil { t.Fatal(err) }
	if err := store.Insert(context.Background(), BuildRecord("second", next, req, time.Unix(2, 0), "private-session")); err != nil { t.Fatal(err) }
	detail, err := store.Detail(context.Background(), "second")
	if err != nil { t.Fatal(err) }
	if string(detail.Payload) != string(next) || !detail.Incremental { t.Fatalf("expected reconstructible delta, got %#v", detail) }
	if strings.Contains(string(detail.Payload), "private-session") { t.Fatal("session identifier was retained in payload") }
	stats, err := store.Statistics(context.Background())
	if err != nil || stats.DeltaCount != 1 || stats.StoredBytes >= stats.ReconstructedBytes { t.Fatalf("unexpected stats %#v err=%v", stats, err) }
}

func TestPromptArchiveDeleteMaterializesDeltaDependentRecord(t *testing.T) {
	store, err := Open(":memory:", Options{})
	if err != nil { t.Fatal(err) }
	defer store.Close()
	first := []byte(`{"message":"prefix"}`)
	second := []byte(`{"message":"prefix with enough additional text to produce a compressed delta"}`)
	req := models.ClaudeRequest{Model: "test"}
	if err := store.Insert(context.Background(), BuildRecord("first", first, req, time.Unix(1, 0), "session")); err != nil { t.Fatal(err) }
	if err := store.Insert(context.Background(), BuildRecord("second", second, req, time.Unix(2, 0), "session")); err != nil { t.Fatal(err) }
	if deleted, err := store.Delete(context.Background(), "first"); err != nil || !deleted { t.Fatalf("delete=%v err=%v", deleted, err) }
	detail, err := store.Detail(context.Background(), "second")
	if err != nil || string(detail.Payload) != string(second) || detail.Incremental { t.Fatalf("dependent was not materialized: %#v err=%v", detail, err) }
}
