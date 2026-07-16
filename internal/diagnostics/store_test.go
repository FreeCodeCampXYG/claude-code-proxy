package diagnostics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreInsertQueryDetailDeleteClearAndExport(t *testing.T) {
	store := openTestStore(t, StoreOptions{})
	ctx := context.Background()
	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{RequestID: "req-1", CreatedAt: base, Method: "POST", Path: "/v1/messages", Provider: "openrouter", Model: "gpt-5", StatusCode: 200, Duration: 1250 * time.Millisecond, Streaming: true, RequestBody: json.RawMessage(`{"model":"gpt-5"}`)},
		{RequestID: "req-2", CreatedAt: base.Add(time.Minute), Method: "POST", Path: "/v1/messages", Provider: "openai", Model: "gpt-5-mini", StatusCode: 500, Error: "upstream failed", Metadata: json.RawMessage(`{"attempt":2}`)},
	}
	for _, event := range events {
		if err := store.Insert(ctx, event); err != nil {
			t.Fatalf("Insert(%s) error = %v", event.RequestID, err)
		}
	}

	summaries, err := store.Query(ctx, Query{Provider: "openrouter", Limit: 10})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(summaries) != 1 || summaries[0].RequestID != "req-1" || summaries[0].Duration != 1250*time.Millisecond {
		t.Fatalf("unexpected summaries: %#v", summaries)
	}

	detail, err := store.Detail(ctx, "req-1")
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if detail.Model != "gpt-5" || string(detail.RequestBody) != `{"model":"gpt-5"}` || !detail.Streaming {
		t.Fatalf("unexpected detail: %#v", detail)
	}

	var exported []string
	if err := store.Export(ctx, Query{Ascending: true}, func(event Event) error {
		exported = append(exported, event.RequestID)
		return nil
	}); err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if fmt.Sprint(exported) != "[req-1 req-2]" {
		t.Fatalf("unexpected export order: %v", exported)
	}

	deleted, err := store.Delete(ctx, "req-1")
	if err != nil || !deleted {
		t.Fatalf("Delete() = %v, %v", deleted, err)
	}
	if _, err := store.Detail(ctx, "req-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Detail(deleted) error = %v, want sql.ErrNoRows", err)
	}
	cleared, err := store.Clear(ctx)
	if err != nil || cleared != 1 {
		t.Fatalf("Clear() = %d, %v", cleared, err)
	}
}

func TestStoreRequestIDIsUnique(t *testing.T) {
	store := openTestStore(t, StoreOptions{})
	ctx := context.Background()
	if err := store.Insert(ctx, Event{RequestID: "same"}); err != nil {
		t.Fatalf("first Insert() error = %v", err)
	}
	if err := store.Insert(ctx, Event{RequestID: "same"}); err == nil {
		t.Fatal("duplicate Insert() expected error")
	}
}

func TestStoreCleanupUsesThreeDayRetention(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, StoreOptions{Now: func() time.Time { return now }})
	ctx := context.Background()
	for _, event := range []Event{
		{RequestID: "expired", CreatedAt: now.Add(-DefaultRetention - time.Millisecond)},
		{RequestID: "boundary", CreatedAt: now.Add(-DefaultRetention)},
		{RequestID: "recent", CreatedAt: now.Add(-time.Hour)},
	} {
		if err := store.Insert(ctx, event); err != nil {
			t.Fatalf("Insert(%s) error = %v", event.RequestID, err)
		}
	}
	removed, err := store.Cleanup(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("Cleanup() = %d, %v", removed, err)
	}
	events, err := store.Query(ctx, Query{Ascending: true})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(events) != 2 || events[0].RequestID != "boundary" || events[1].RequestID != "recent" {
		t.Fatalf("unexpected retained events: %#v", events)
	}
}

func TestStoreConcurrentWrites(t *testing.T) {
	store := openTestStore(t, StoreOptions{})
	ctx := context.Background()
	const writers = 32
	var wg sync.WaitGroup
	errorsCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errorsCh <- store.Insert(ctx, Event{RequestID: fmt.Sprintf("req-%02d", i), Model: "gpt-5"})
		}(i)
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent Insert() error = %v", err)
		}
	}
	events, err := store.Query(ctx, Query{})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(events) != writers {
		t.Fatalf("event count = %d, want %d", len(events), writers)
	}
}

func TestStoreEnablesWALAndBusyTimeout(t *testing.T) {
	store := openTestStore(t, StoreOptions{BusyTimeout: 2300 * time.Millisecond})
	var journalMode string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
	var busyTimeout int
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout != 2300 {
		t.Fatalf("busy_timeout = %d, want 2300", busyTimeout)
	}
}

func openTestStore(t *testing.T, options StoreOptions) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "diagnostics.db"), options)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}
