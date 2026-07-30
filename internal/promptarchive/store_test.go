package promptarchive

import (
	"context"
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
	if len(detail.Payload) == 0 || detail.Model != "gpt-test" {
		t.Fatalf("unexpected detail: %#v", detail)
	}
	deleted, err := store.Delete(context.Background(), "req-1")
	if err != nil || !deleted {
		t.Fatalf("Delete() deleted=%v err=%v", deleted, err)
	}
}
