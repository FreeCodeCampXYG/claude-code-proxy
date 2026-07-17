package diagnostics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreContentRetentionAndPartialSummary(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, StoreOptions{Retention: 2 * time.Hour, ContentRetention: time.Hour, CaptureContent: true, Now: func() time.Time { return now }})
	if err := store.Insert(t.Context(), Event{RequestID: "content-event", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"payload":"` + strings.Repeat("中", DefaultMaxContentBytes) + `","path":"C:\\private\\project\\file.go","api_key":"sk-secret"}`)
	content, mode, err := RepresentJSONContent(body, ContentRepresentationOptions{MaxBytes: 128, PreviewBytes: 16, SourceBytes: len(body)})
	if err != nil || mode != ContentCaptureSummary || !json.Valid(content) {
		t.Fatalf("RepresentJSONContent() = %s, %q, %v", content, mode, err)
	}
	var summary PartialJSONSummary
	if err := json.Unmarshal(content, &summary); err != nil || summary.OmittedChars <= 0 || summary.DisplayedChars <= 0 || summary.Reason != "content_limit" {
		t.Fatalf("partial summary = %#v, %v", summary, err)
	}
	if err := store.InsertContent(t.Context(), ContentSnapshot{RequestID: "content-event", Boundary: ContentBoundaryClaudeRequest, Content: content, CaptureMode: mode}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.Content(t.Context(), ContentQuery{RequestID: "content-event"})
	if err != nil || len(snapshots) != 1 || snapshots[0].CaptureMode != ContentCaptureSummary {
		t.Fatalf("Content() = %#v, %v", snapshots, err)
	}
	if _, err := store.CleanupBefore(t.Context(), now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if snapshots, err := store.Content(t.Context(), ContentQuery{RequestID: "content-event"}); err != nil || len(snapshots) != 1 {
		t.Fatalf("cleanup should retain current content: %#v, %v", snapshots, err)
	}
}

func TestPrepareSnapshotsRedactsLargeContentAndPaths(t *testing.T) {
	store := openTestStore(t, StoreOptions{CaptureContent: true})
	body := []byte(`{"prompt":"` + strings.Repeat("x", 1400*1024) + `","workspace":"\\\\server\\share\\repo","arguments":"{\"api_key\":\"sk-secret\",\"file_path\":\"/private/source/main.go\"}"}`)
	snapshots := store.prepareSnapshots([]ContentCapture{{RequestID: "large", Boundary: ContentBoundaryClaudeRequest, SourceBytes: len(body), Body: body}})
	if len(snapshots) != 1 || snapshots[0].CaptureMode != ContentCaptureSummary {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	text := string(snapshots[0].Content)
	if strings.Contains(text, "sk-secret") || strings.Contains(text, "server\\share") || strings.Contains(text, "/private/source") || strings.Contains(text, "sha256") {
		t.Fatalf("unsafe partial snapshot: %s", text)
	}
	var summary PartialJSONSummary
	if err := json.Unmarshal(snapshots[0].Content, &summary); err != nil || summary.OmittedChars == 0 || summary.SourceBytes != len(body) {
		t.Fatalf("summary = %#v, %v", summary, err)
	}
}

func TestStoreInsertBundleRollsBackEventWhenContentInsertFails(t *testing.T) {
	store := openTestStore(t, StoreOptions{CaptureContent: true})
	if _, err := store.db.Exec(`CREATE TRIGGER reject_diagnostics_content
		BEFORE INSERT ON diagnostics_content
		BEGIN
			SELECT RAISE(ABORT, 'reject content');
		END;`); err != nil {
		t.Fatal(err)
	}

	err := store.insertBundle(t.Context(), Event{RequestID: "atomic-bundle"}, []ContentSnapshot{{
		RequestID: "atomic-bundle",
		Boundary:  ContentBoundaryClaudeRequest,
		Content:   json.RawMessage(`{"safe":true}`),
	}})
	if err == nil || !strings.Contains(err.Error(), "reject content") {
		t.Fatalf("insertBundle() error = %v, want content insertion failure", err)
	}
	if _, err := store.Detail(t.Context(), "atomic-bundle"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Detail() error = %v, want sql.ErrNoRows after rollback", err)
	}
}

func TestStoreInsertBundlePersistsEventAndSnapshots(t *testing.T) {
	store := openTestStore(t, StoreOptions{CaptureContent: true})
	snapshots := []ContentSnapshot{
		{RequestID: "complete-bundle", Boundary: ContentBoundaryClaudeRequest, Content: json.RawMessage(`{"request":true}`)},
		{RequestID: "complete-bundle", Boundary: ContentBoundaryUpstreamResponse, Content: json.RawMessage(`{"response":true}`)},
	}
	if err := store.insertBundle(t.Context(), Event{RequestID: "complete-bundle", Model: "gpt-5"}, snapshots); err != nil {
		t.Fatalf("insertBundle() error = %v", err)
	}
	if event, err := store.Detail(t.Context(), "complete-bundle"); err != nil || event.Model != "gpt-5" {
		t.Fatalf("Detail() = %#v, %v", event, err)
	}
	stored, err := store.Content(t.Context(), ContentQuery{RequestID: "complete-bundle"})
	if err != nil || len(stored) != len(snapshots) {
		t.Fatalf("Content() = %#v, %v", stored, err)
	}
}

func TestStoreInsertBundleRejectsMismatchedSnapshotBeforeWritingEvent(t *testing.T) {
	store := openTestStore(t, StoreOptions{CaptureContent: true})
	err := store.insertBundle(t.Context(), Event{RequestID: "event-request"}, []ContentSnapshot{{
		RequestID: "content-request",
		Boundary:  ContentBoundaryClaudeRequest,
		Content:   json.RawMessage(`{"safe":true}`),
	}})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("insertBundle() error = %v, want request ID mismatch", err)
	}
	if _, err := store.Detail(t.Context(), "event-request"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Detail() error = %v, want sql.ErrNoRows", err)
	}
}

func TestStoreEnqueueAfterCloseReleasesCaptures(t *testing.T) {
	store := openTestStore(t, StoreOptions{})
	if !store.ReserveCapture(16) {
		t.Fatal("ReserveCapture() = false")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	queued := store.Enqueue(Event{RequestID: "after-close"}, []ContentCapture{{ReservedBytes: 16}})
	if queued {
		t.Fatal("Enqueue() = true after Close()")
	}
	if bytes := store.captureBytes.Load(); bytes != 0 {
		t.Fatalf("reserved capture bytes = %d, want 0", bytes)
	}
}

func TestStoreClosePersistsAcceptedBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	store, err := Open(path, StoreOptions{CaptureContent: true})
	if err != nil {
		t.Fatal(err)
	}
	if !store.Enqueue(Event{RequestID: "before-close"}, []ContentCapture{{
		RequestID: "before-close",
		Boundary:  ContentBoundaryClaudeRequest,
		Body:      []byte(`{"safe":true}`),
		SourceBytes: len(`{"safe":true}`),
	}}) {
		t.Fatal("Enqueue() = false")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path, StoreOptions{CaptureContent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Detail(t.Context(), "before-close"); err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	snapshots, err := store.Content(t.Context(), ContentQuery{RequestID: "before-close"})
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("Content() = %#v, %v", snapshots, err)
	}
}

func TestStoreInsertQueryDetailDeleteClearAndExport(t *testing.T) {
	store := openTestStore(t, StoreOptions{})
	ctx := context.Background()
	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{RequestID: "req-1", CreatedAt: base, Method: "POST", Path: "/v1/messages", Provider: "openrouter", Model: "gpt-5", APIKeyLabel: "key-1", StatusCode: 200, Duration: 1250 * time.Millisecond, Streaming: true, RequestBody: json.RawMessage(`{"model":"gpt-5"}`)},
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
	if len(summaries) != 1 || summaries[0].RequestID != "req-1" || summaries[0].Duration != 1250*time.Millisecond || summaries[0].APIKeyLabel != "key-1" {
		t.Fatalf("unexpected summaries: %#v", summaries)
	}

	detail, err := store.Detail(ctx, "req-1")
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if detail.Model != "gpt-5" || detail.APIKeyLabel != "key-1" || string(detail.RequestBody) != `{"model":"gpt-5"}` || !detail.Streaming {
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

func TestStoreMigratesV1PreservingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE diagnostics_events (
		request_id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		method TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '', status_code INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0,
		streaming INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', request_body BLOB, response_body BLOB, metadata BLOB
	); INSERT INTO diagnostics_events(request_id,created_at,updated_at,model,status_code) VALUES('legacy',1,2,'old-model',200); PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	store, err := Open(path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event, err := store.Detail(t.Context(), "legacy")
	if err != nil || event.Model != "old-model" || event.AttemptCount != 0 {
		t.Fatalf("migrated event = %#v, %v", event, err)
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}

func TestStoreRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	db, err := sql.Open("sqlite", path)
	if err != nil { t.Fatal(err) }
	if _, err := db.Exec("PRAGMA user_version=5"); err != nil { t.Fatal(err) }
	_ = db.Close()
	if _, err := Open(path, StoreOptions{}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Open() error = %v, want newer schema rejection", err)
	}
}

func TestStoreAnalyticsAggregatesNormalizedFields(t *testing.T) {
	store := openTestStore(t, StoreOptions{})
	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{RequestID: "ok", CreatedAt: base, Provider: "newapi", Model: "gpt", APIKeyLabel: "key-1", StatusCode: 200, Duration: 100 * time.Millisecond, Streaming: true, AttemptCount: 2, RetryCount: 1, InputTokens: 10, OutputTokens: 20, CacheReadInputTokens: 3, CacheCreationInputTokens: 2, CompletionState: "completed", TaskHash: "task-a"},
		{RequestID:"truncated",CreatedAt:base.Add(time.Hour),Provider:"newapi",Model:"gpt",APIKeyLabel:"key-2",StatusCode:502,Duration:300*time.Millisecond,Streaming:true,AttemptCount:1,InputTokens:4,OutputTokens:5,CompletionState:"truncated",FailureKind:"truncated",Truncated:true},
		{RequestID:"canceled",CreatedAt:base.Add(time.Hour),Provider:"openai",Model:"mini",APIKeyLabel:"key-2",StatusCode:502,Duration:200*time.Millisecond,AttemptCount:1,CompletionState:"canceled",FailureKind:"canceled",Canceled:true},
	}
	for _, event := range events { if err := store.Insert(t.Context(), event); err != nil { t.Fatal(err) } }
	a, err := store.Analytics(t.Context(), Query{Since:base, Until:base.Add(2*time.Hour)})
	if err != nil { t.Fatal(err) }
	if a.Total != 3 || a.Success != 1 || a.Failure != 2 || a.Truncated != 1 || a.Canceled != 1 || a.Streaming != 2 || a.Retries != 1 || a.Attempts != 4 || a.AverageAttempts != 4.0/3.0 || a.InputTokens != 14 || a.OutputTokens != 25 || a.CacheReadInputTokens != 3 || a.CacheCreationInputTokens != 2 || a.AverageLatencyMS != 200 || a.P95LatencyMS != 300 || a.TaskGroupingUnavailable != 2 {
		t.Fatalf("unexpected analytics: %#v", a)
	}
	if len(a.HourlyTimeline) != 2 || len(a.ByModel) != 2 || len(a.ByProvider) != 2 || len(a.ByAPIKey) != 2 || len(a.ByCompletionState) != 3 || len(a.TaskGroups) != 1 {
		t.Fatalf("unexpected breakdowns: %#v", a)
	}
}

func TestStoreRepairsMissingV2Indexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	store, err := Open(path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX idx_diagnostics_events_model`); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	store, err = Open(path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_diagnostics_events_model'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repaired index count = %d, %v", count, err)
	}
}

func TestStoreRejectsIncompatibleV2Schema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE diagnostics_events (request_id TEXT PRIMARY KEY); PRAGMA user_version=2;`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(path, StoreOptions{}); err == nil || !strings.Contains(err.Error(), "missing required columns") {
		t.Fatalf("Open() error = %v, want incompatible column rejection", err)
	}
}

func TestStoreRejectsIncompatibleV2Index(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	store, err := Open(path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX idx_diagnostics_events_model; CREATE INDEX idx_diagnostics_events_model ON diagnostics_events(provider)`); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if _, err := Open(path, StoreOptions{}); err == nil || !strings.Contains(err.Error(), "index idx_diagnostics_events_model has columns") {
		t.Fatalf("Open() error = %v, want incompatible index rejection", err)
	}
}

func TestStoreAnalyticsPreservesOmittedBounds(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.FixedZone("offset", 8*60*60))
	store := openTestStore(t, StoreOptions{Retention: time.Hour, Now: func() time.Time { return now }})
	old := now.UTC().Add(-2 * time.Hour)
	for _, event := range []Event{{RequestID: "old", CreatedAt: old}, {RequestID: "retained", CreatedAt: now.UTC()}} {
		if err := store.Insert(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	analytics, err := store.Analytics(t.Context(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if analytics.Total != 2 || !analytics.Since.IsZero() || !analytics.Until.IsZero() {
		t.Fatalf("unbounded analytics = %#v, want both events with zero bounds", analytics)
	}
	events, err := store.Query(t.Context(), Query{Ascending: true})
	if err != nil {
		t.Fatal(err)
	}
	var exported []string
	if err := store.Export(t.Context(), Query{Ascending: true}, func(event Event) error {
		exported = append(exported, event.RequestID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != analytics.Total || fmt.Sprint(exported) != "[old retained]" {
		t.Fatalf("all-results mismatch: analytics=%d events=%#v export=%v", analytics.Total, events, exported)
	}
}

func TestStoreAnalyticsReturnsExplicitUTCBounds(t *testing.T) {
	store := openTestStore(t, StoreOptions{})
	since := time.Date(2026, 7, 16, 18, 0, 0, 0, time.FixedZone("offset", 8*60*60))
	until := since.Add(time.Hour)
	analytics, err := store.Analytics(t.Context(), Query{Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	wantSince := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	wantUntil := wantSince.Add(time.Hour)
	if analytics.Since != wantSince || analytics.Until != wantUntil || analytics.Since.Location() != time.UTC || analytics.Until.Location() != time.UTC {
		t.Fatalf("explicit bounds = %v..%v, want %v..%v UTC", analytics.Since, analytics.Until, wantSince, wantUntil)
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
