package diagnostics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	DefaultRetention             = 72 * time.Hour
	DefaultContentRetention      = time.Hour
	DefaultMaxContentBytes     = 64 * 1024
	DefaultContentPreviewBytes = 1024
	DefaultCaptureBytes        = 2 * 1024 * 1024
	DefaultCaptureInFlightBytes = 8 * 1024 * 1024
)

type StoreOptions struct {
	Retention        time.Duration
	ContentRetention time.Duration
	CaptureContent   bool
	BusyTimeout      time.Duration
	Now              func() time.Time
}

type Store struct {
	db               *sql.DB
	retention        time.Duration
	contentRetention time.Duration
	captureContent   bool
	now              func() time.Time
	jobs             chan persistJob
	stop             chan struct{}
	worker           sync.WaitGroup
	dropped          atomic.Uint64
	captureBytes     atomic.Int64
	captureByteLimit int64
	lifecycleMu      sync.Mutex
	closing          bool
}

type persistJob struct {
	event    Event
	captures []ContentCapture
}

// ContentCapture is an in-memory handoff to the diagnostics worker. It is
// deliberately not persisted as-is: the worker removes secret values and
// bounds the representation before writing SQLite.
type ContentCapture struct {
	RequestID     string
	AttemptNumber int
	Boundary      ContentBoundary
	CreatedAt     time.Time
	CaptureMode   ContentCaptureMode
	Body          []byte
	SourceBytes   int
	SourceTruncated bool
	Reason        string
	ReservedBytes int64
}

type ContentBoundary string

const (
	ContentBoundaryClaudeRequest  ContentBoundary = "claude_request"
	ContentBoundaryUpstreamRequest ContentBoundary = "upstream_request"
	ContentBoundaryUpstreamResponse ContentBoundary = "upstream_response"
	ContentBoundaryClaudeResponse ContentBoundary = "claude_response"
)

type ContentCaptureMode string

const (
	ContentCaptureFull    ContentCaptureMode = "full"
	ContentCaptureSummary ContentCaptureMode = "partial_summary"
)

type ContentSnapshot struct {
	RequestID     string             `json:"request_id"`
	AttemptNumber int                `json:"attempt_number"`
	Boundary      ContentBoundary    `json:"boundary"`
	CreatedAt     time.Time          `json:"created_at"`
	ExpiresAt     time.Time          `json:"expires_at"`
	CaptureMode   ContentCaptureMode `json:"capture_mode"`
	Content       json.RawMessage    `json:"content"`
}

type ContentQuery struct {
	RequestID     string
	AttemptNumber *int
	Boundary      ContentBoundary
}

type ContentRepresentationOptions struct {
	MaxBytes        int
	PreviewBytes    int
	SourceBytes     int
	Reason          string
	SourceTruncated bool
}

type PartialJSONSummary struct {
	Partial          bool   `json:"partial"`
	Reason           string `json:"reason"`
	SourceBytes      int    `json:"source_bytes"`
	SourceTruncated  bool   `json:"source_truncated,omitempty"`
	DisplayedChars   int    `json:"displayed_characters"`
	OmittedChars     int    `json:"omitted_characters"`
	Head             string `json:"head"`
	Tail             string `json:"tail"`
}

type Event struct {
	RequestID       string          `json:"request_id"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	Method          string          `json:"method,omitempty"`
	Path            string          `json:"path,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	Model           string          `json:"model,omitempty"`
	StatusCode      int             `json:"status_code,omitempty"`
	Duration        time.Duration   `json:"duration,omitempty"`
	Streaming       bool            `json:"streaming"`
	Error           string          `json:"error,omitempty"`
	RequestBody     json.RawMessage `json:"request_body,omitempty"`
	ResponseBody    json.RawMessage `json:"response_body,omitempty"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
	AttemptCount             int             `json:"attempt_count"`
	RetryCount               int             `json:"retry_count"`
	InputTokens              int             `json:"input_tokens"`
	OutputTokens             int             `json:"output_tokens"`
	CacheReadInputTokens     int             `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int             `json:"cache_creation_input_tokens"`
	ChunkCount               int             `json:"chunk_count"`
	StopReason               string          `json:"stop_reason,omitempty"`
	CompletionState string          `json:"completion_state,omitempty"`
	FailureKind     string          `json:"failure_kind,omitempty"`
	Canceled        bool            `json:"canceled"`
	Truncated       bool            `json:"truncated"`
	APIKeyLabel              string          `json:"api_key_label,omitempty"`
	TaskHash                 string          `json:"task_hash,omitempty"`
	ClaudeRequestBytes       int             `json:"claude_request_bytes"`
	UpstreamRequestBytes     int             `json:"upstream_request_bytes"`
	UpstreamResponseBytes    int             `json:"upstream_response_bytes"`
	ClaudeResponseBytes      int             `json:"claude_response_bytes"`
	MessageCount             int             `json:"message_count"`
	ToolCount                int             `json:"tool_count"`
}

type EventSummary struct {
	RequestID       string        `json:"request_id"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	Method          string        `json:"method,omitempty"`
	Path            string        `json:"path,omitempty"`
	Provider        string        `json:"provider,omitempty"`
	Model           string        `json:"model,omitempty"`
	StatusCode      int           `json:"status_code,omitempty"`
	Duration        time.Duration `json:"duration,omitempty"`
	Streaming       bool          `json:"streaming"`
	Error           string        `json:"error,omitempty"`
	AttemptCount             int           `json:"attempt_count"`
	RetryCount               int           `json:"retry_count"`
	InputTokens              int           `json:"input_tokens"`
	OutputTokens             int           `json:"output_tokens"`
	CacheReadInputTokens     int           `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int           `json:"cache_creation_input_tokens"`
	ChunkCount               int           `json:"chunk_count"`
	StopReason               string        `json:"stop_reason,omitempty"`
	CompletionState string        `json:"completion_state,omitempty"`
	FailureKind     string        `json:"failure_kind,omitempty"`
	Canceled        bool          `json:"canceled"`
	Truncated       bool          `json:"truncated"`
	APIKeyLabel              string        `json:"api_key_label,omitempty"`
	TaskHash                 string        `json:"task_hash,omitempty"`
	ClaudeRequestBytes       int           `json:"claude_request_bytes"`
	UpstreamRequestBytes     int           `json:"upstream_request_bytes"`
	UpstreamResponseBytes    int           `json:"upstream_response_bytes"`
	ClaudeResponseBytes      int           `json:"claude_response_bytes"`
	MessageCount             int           `json:"message_count"`
	ToolCount                int           `json:"tool_count"`
}

type Query struct {
	RequestID       string
	Model           string
	Provider        string
	CompletionState string
	Streaming       *bool
	Since           time.Time
	Until           time.Time
	Limit           int
	Offset          int
	Ascending       bool
}

type Analytics struct {
	Since                   time.Time        `json:"since,omitempty"`
	Until                   time.Time        `json:"until,omitempty"`
	Total                   int              `json:"total"`
	Success                 int              `json:"success"`
	Failure                 int              `json:"failure"`
	Truncated               int              `json:"truncated"`
	Canceled                int              `json:"canceled"`
	Streaming               int              `json:"streaming"`
	Retries                 int              `json:"retries"`
	Attempts                 int              `json:"attempts"`
	AverageAttempts          float64          `json:"average_attempts"`
	InputTokens              int64            `json:"input_tokens"`
	OutputTokens             int64            `json:"output_tokens"`
	CacheReadInputTokens     int64            `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64            `json:"cache_creation_input_tokens"`
	ClaudeRequestBytes       int64            `json:"claude_request_bytes"`
	UpstreamRequestBytes     int64            `json:"upstream_request_bytes"`
	UpstreamResponseBytes    int64            `json:"upstream_response_bytes"`
	ClaudeResponseBytes      int64            `json:"claude_response_bytes"`
	AverageClaudeRequestBytes float64         `json:"average_claude_request_bytes"`
	AverageTotalTokens       float64          `json:"average_total_tokens"`
	BytesPerInputToken       float64          `json:"bytes_per_input_token"`
	BytesPerOutputToken      float64          `json:"bytes_per_output_token"`
	AverageLatencyMS        float64          `json:"average_latency_ms"`
	P95LatencyMS            int64            `json:"p95_latency_ms"`
	TokenByteCorrelation    TokenByteCorrelation `json:"token_byte_correlation"`
	ModelConsumption        []ModelConsumption   `json:"model_consumption"`
	ByModel                 []AnalyticsCount `json:"by_model"`
	ByProvider              []AnalyticsCount `json:"by_provider"`
	ByAPIKey                []AnalyticsCount `json:"by_api_key"`
	ByCompletionState       []AnalyticsCount `json:"by_completion_state"`
	HourlyTimeline          []AnalyticsHour  `json:"hourly_timeline"`
	TaskGroups              []TaskGroup      `json:"task_groups"`
	TaskGroupingUnavailable int              `json:"task_grouping_unavailable"`
}

type AnalyticsCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type AnalyticsHour struct {
	Hour         time.Time `json:"hour"`
	Total        int       `json:"total"`
	Success      int       `json:"success"`
	Failure      int       `json:"failure"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	ClaudeRequestBytes int64 `json:"claude_request_bytes"`
	UpstreamRequestBytes int64 `json:"upstream_request_bytes"`
	UpstreamResponseBytes int64 `json:"upstream_response_bytes"`
	ClaudeResponseBytes int64 `json:"claude_response_bytes"`
}


type TokenByteCorrelation struct {
	SampleCount                 int     `json:"sample_count"`
	InputTokenVsRequestBytes    float64 `json:"input_token_vs_request_bytes"`
	OutputTokenVsResponseBytes  float64 `json:"output_token_vs_response_bytes"`
	TotalTokenVsTotalBytes      float64 `json:"total_token_vs_total_bytes"`
	Status                      string  `json:"status"`
}

type ModelConsumption struct {
	Model                    string  `json:"model"`
	Count                    int     `json:"count"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	TotalTokens              int64   `json:"total_tokens"`
	ClaudeRequestBytes       int64   `json:"claude_request_bytes"`
	UpstreamResponseBytes    int64   `json:"upstream_response_bytes"`
	AverageLatencyMS         float64 `json:"average_latency_ms"`
	BytesPerToken            float64 `json:"bytes_per_token"`
}

type TaskGroup struct {
	TaskHash string `json:"task_hash"`
	Count    int    `json:"count"`
}

func Open(path string, options StoreOptions) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("diagnostics database path is required")
	}
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve diagnostics database path: %w", err)
		}
		path = absolute
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, fmt.Errorf("create diagnostics database directory: %w", err)
		}
	}
	busyTimeout := options.BusyTimeout
	if busyTimeout <= 0 {
		busyTimeout = 5 * time.Second
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open diagnostics database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{
		db:               db,
		retention:        options.Retention,
		contentRetention: options.ContentRetention,
		captureContent:   options.CaptureContent,
		now:              options.Now,
		captureByteLimit: DefaultCaptureInFlightBytes,
	}
	if store.retention <= 0 {
		store.retention = DefaultRetention
	}
	if store.contentRetention <= 0 {
		store.contentRetention = DefaultContentRetention
	}
	if store.contentRetention > store.retention {
		store.contentRetention = store.retention
	}
	if store.now == nil {
		store.now = time.Now
	}
	if err := store.initialize(context.Background(), busyTimeout, path != ":memory:"); err != nil {
		db.Close()
		return nil, err
	}
	if !store.captureContent {
		if _, err := store.db.ExecContext(context.Background(), "DELETE FROM diagnostics_content"); err != nil {
			db.Close()
			return nil, fmt.Errorf("purge disabled diagnostics content: %w", err)
		}
	}
	store.startWriter()
	return store, nil
}

func (store *Store) initialize(ctx context.Context, busyTimeout time.Duration, enableWAL bool) error {
	if err := store.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping diagnostics database: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set diagnostics busy timeout: %w", err)
	}
	if enableWAL {
		if _, err := store.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
			return fmt.Errorf("enable diagnostics WAL mode: %w", err)
		}
	}
	if _, err := store.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable diagnostics foreign keys: %w", err)
	}
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read diagnostics schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("diagnostics schema version %d is newer than supported version %d", version, schemaVersion)
	}
	switch version {
	case 0:
		if _, err := store.db.ExecContext(ctx, createSchemaV5SQL); err != nil {
			return fmt.Errorf("create diagnostics schema: %w", err)
		}
	case 1:
		if err := store.migrate(ctx, migrateV1ToV2Statements, "v1 to v2"); err != nil {
			return err
		}
		if err := store.migrate(ctx, migrateV2ToV3Statements, "v2 to v3"); err != nil {
			return err
		}
		if err := store.migrate(ctx, migrateV3ToV4Statements, "v3 to v4"); err != nil {
			return err
		}
		if err := store.migrate(ctx, migrateV4ToV5Statements, "v4 to v5"); err != nil {
			return err
		}
	case 2:
		if err := store.migrate(ctx, migrateV2ToV3Statements, "v2 to v3"); err != nil {
			return err
		}
		if err := store.migrate(ctx, migrateV3ToV4Statements, "v3 to v4"); err != nil {
			return err
		}
		if err := store.migrate(ctx, migrateV4ToV5Statements, "v4 to v5"); err != nil {
			return err
		}
	case 3:
		if err := store.migrate(ctx, migrateV3ToV4Statements, "v3 to v4"); err != nil {
			return err
		}
		if err := store.migrate(ctx, migrateV4ToV5Statements, "v4 to v5"); err != nil {
			return err
		}
	case 4:
		if err := store.migrate(ctx, migrateV4ToV5Statements, "v4 to v5"); err != nil {
			return err
		}
	}
	return store.validateAndRepairSchemaV5(ctx)
}

func (store *Store) migrate(ctx context.Context, statements []string, label string) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin diagnostics schema migration %s: %w", label, err)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migrate diagnostics schema %s: %w", label, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit diagnostics schema migration %s: %w", label, err)
	}
	return nil
}

var requiredSchemaV5Columns = []string{
	"request_id", "created_at", "updated_at", "method", "path", "provider", "model",
	"status_code", "duration_ms", "streaming", "error", "request_body", "response_body", "metadata",
	"attempt_count", "retry_count", "input_tokens", "output_tokens", "cache_read_input_tokens",
	"cache_creation_input_tokens", "chunk_count", "stop_reason", "completion_state", "failure_kind",
	"canceled", "truncated", "api_key_label", "task_hash", "claude_request_bytes",
	"upstream_request_bytes", "upstream_response_bytes", "claude_response_bytes", "message_count", "tool_count",
}

var requiredSchemaV5Indexes = map[string]struct {
	columns   []string
	statement string
}{
	"idx_diagnostics_events_created_at": {columns: []string{"created_at", "request_id"}, statement: `CREATE INDEX idx_diagnostics_events_created_at ON diagnostics_events(created_at DESC, request_id DESC)`},
	"idx_diagnostics_events_model":      {columns: []string{"model", "created_at"}, statement: `CREATE INDEX idx_diagnostics_events_model ON diagnostics_events(model, created_at DESC)`},
	"idx_diagnostics_events_task_hash":  {columns: []string{"task_hash", "created_at"}, statement: `CREATE INDEX idx_diagnostics_events_task_hash ON diagnostics_events(task_hash, created_at DESC)`},
	"idx_diagnostics_content_created_at": {columns: []string{"created_at", "request_id", "attempt_number", "boundary"}, statement: `CREATE INDEX idx_diagnostics_content_created_at ON diagnostics_content(created_at DESC, request_id DESC, attempt_number DESC, boundary)`},
}

func (store *Store) validateAndRepairSchemaV5(ctx context.Context) error {
	rows, err := store.db.QueryContext(ctx, "PRAGMA table_info(diagnostics_events)")
	if err != nil {
		return fmt.Errorf("inspect diagnostics schema v5 columns: %w", err)
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan diagnostics schema v5 columns: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate diagnostics schema v5 columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close diagnostics schema v5 column inspection: %w", err)
	}
	var missing []string
	for _, name := range requiredSchemaV5Columns {
		if !columns[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("diagnostics schema v5 is incompatible: missing required columns: %s", strings.Join(missing, ", "))
	}

	for name, required := range requiredSchemaV5Indexes {
		var found string
		err := store.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := store.db.ExecContext(ctx, required.statement); err != nil {
				return fmt.Errorf("repair diagnostics schema v5 index %s: %w", name, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect diagnostics schema v5 index %s: %w", name, err)
		}
		indexRows, err := store.db.QueryContext(ctx, "PRAGMA index_info("+name+")")
		if err != nil {
			return fmt.Errorf("inspect diagnostics schema v5 index %s columns: %w", name, err)
		}
		var actual []string
		for indexRows.Next() {
			var sequence, cid int
			var column string
			if err := indexRows.Scan(&sequence, &cid, &column); err != nil {
				_ = indexRows.Close()
				return fmt.Errorf("scan diagnostics schema v5 index %s columns: %w", name, err)
			}
			actual = append(actual, column)
		}
		if err := indexRows.Err(); err != nil {
			_ = indexRows.Close()
			return fmt.Errorf("iterate diagnostics schema v5 index %s columns: %w", name, err)
		}
		if err := indexRows.Close(); err != nil {
			return fmt.Errorf("close diagnostics schema v5 index %s inspection: %w", name, err)
		}
		if !sameStrings(actual, required.columns) {
			return fmt.Errorf("diagnostics schema v5 is incompatible: index %s has columns %v, want %v", name, actual, required.columns)
		}
	}
	return nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (store *Store) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	store.lifecycleMu.Lock()
	if store.closing {
		store.lifecycleMu.Unlock()
		return nil
	}
	store.closing = true
	if store.stop != nil {
		close(store.stop)
	}
	store.lifecycleMu.Unlock()

	store.worker.Wait()
	store.lifecycleMu.Lock()
	store.jobs, store.stop = nil, nil
	store.lifecycleMu.Unlock()
	return store.db.Close()
}

func (store *Store) startWriter() {
	store.jobs = make(chan persistJob, 64)
	store.stop = make(chan struct{})
	store.worker.Add(1)
	go func() {
		defer store.worker.Done()
		cleanupTicker := time.NewTicker(time.Minute)
		defer cleanupTicker.Stop()
		for {
			select {
			case job := <-store.jobs:
				store.persistJob(job)
			case <-cleanupTicker.C:
				store.cleanupExpiredContent()
			case <-store.stop:
				for {
					select {
					case job := <-store.jobs:
						store.persistJob(job)
					default:
						return
					}
				}
			}
		}
	}()
}

func (store *Store) persistJob(job persistJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer store.releaseCaptures(job.captures)

	snapshots := store.prepareSnapshots(job.captures)
	if err := store.insertBundle(ctx, job.event, snapshots); err != nil {
		store.dropped.Add(1)
		fmt.Printf("[WARN] Diagnostics bundle persistence failed; dropped request_id=%s: %v\n", job.event.RequestID, err)
	}
	store.cleanupExpiredContentContext(ctx)
}

func (store *Store) cleanupExpiredContent() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	store.cleanupExpiredContentContext(ctx)
}

func (store *Store) cleanupExpiredContentContext(ctx context.Context) {
	if _, err := store.db.ExecContext(ctx, "DELETE FROM diagnostics_content WHERE expires_at <= ?", toMillis(store.now().UTC())); err != nil {
		store.dropped.Add(1)
		fmt.Printf("[WARN] Diagnostics content cleanup failed: %v\n", err)
	}
}

// Enqueue records a diagnostic bundle without waiting for SQLite. A full queue simply drops
// the diagnostic record; callers must never let observability alter proxy behavior.
func (store *Store) Enqueue(event Event, captures []ContentCapture) bool {
	if store == nil {
		return false
	}
	store.lifecycleMu.Lock()
	defer store.lifecycleMu.Unlock()
	if store.closing || store.jobs == nil {
		store.releaseCaptures(captures)
		return false
	}
	job := persistJob{event: event, captures: append([]ContentCapture(nil), captures...)}
	select {
	case store.jobs <- job:
		return true
	default:
		store.releaseCaptures(captures)
		store.dropped.Add(1)
		return false
	}
}

// ReserveCapture reserves deferred diagnostics memory without blocking proxy work.
func (store *Store) ReserveCapture(size int) bool {
	if store == nil || size <= 0 || int64(size) > DefaultCaptureBytes {
		return false
	}
	for {
		current := store.captureBytes.Load()
		if current+int64(size) > store.captureByteLimit {
			return false
		}
		if store.captureBytes.CompareAndSwap(current, current+int64(size)) {
			return true
		}
	}
}

func (store *Store) releaseCaptures(captures []ContentCapture) {
	for _, capture := range captures {
		if capture.ReservedBytes > 0 {
			store.captureBytes.Add(-capture.ReservedBytes)
		}
	}
}

func (store *Store) Dropped() uint64 {
	if store == nil {
		return 0
	}
	return store.dropped.Load()
}

func (store *Store) prepareSnapshots(captures []ContentCapture) []ContentSnapshot {
	if !store.captureContent {
		return nil
	}
	snapshots := make([]ContentSnapshot, 0, len(captures))
	for _, capture := range captures {
		if !validContentBoundary(capture.Boundary) {
			continue
		}
		content, mode := captureSummary(capture)
		if len(capture.Body) > 0 {
			safeBody, err := RedactSecretsJSON(capture.Body)
			if err != nil {
				capture.Reason = "malformed_json"
				content, mode = captureSummary(capture)
			} else {
				content, mode, err = RepresentJSONContent(safeBody, ContentRepresentationOptions{
					MaxBytes: DefaultMaxContentBytes, SourceBytes: capture.SourceBytes,
					Reason: capture.Reason, SourceTruncated: capture.SourceTruncated,
				})
				if err != nil {
					capture.Reason = "malformed_json"
					content, mode = captureSummary(capture)
				}
			}
		}
		if capture.CaptureMode == ContentCaptureSummary {
			mode = ContentCaptureSummary
		}
		snapshots = append(snapshots, ContentSnapshot{
			RequestID:     capture.RequestID,
			AttemptNumber: capture.AttemptNumber,
			Boundary:      capture.Boundary,
			CreatedAt:     capture.CreatedAt,
			CaptureMode:   mode,
			Content:       content,
		})
	}
	return snapshots
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (store *Store) normalizeEvent(event Event) (Event, error) {
	if strings.TrimSpace(event.RequestID) == "" {
		return Event{}, errors.New("diagnostics request ID is required")
	}
	now := store.now().UTC()
	if event.CreatedAt.IsZero() {
		event.CreatedAt = now
	}
	if event.UpdatedAt.IsZero() {
		event.UpdatedAt = event.CreatedAt
	}
	return event, nil
}

func (store *Store) insertEvent(ctx context.Context, executor sqlExecutor, event Event) error {
	_, err := executor.ExecContext(ctx, `
		INSERT INTO diagnostics_events (
			request_id, created_at, updated_at, method, path, provider, model,
			status_code, duration_ms, streaming, error, request_body, response_body, metadata,
			attempt_count, retry_count, input_tokens, output_tokens, cache_read_input_tokens,
			cache_creation_input_tokens, chunk_count, stop_reason, completion_state, failure_kind,
			canceled, truncated, api_key_label, task_hash, claude_request_bytes, upstream_request_bytes,
			upstream_response_bytes, claude_response_bytes, message_count, tool_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.RequestID, toMillis(event.CreatedAt), toMillis(event.UpdatedAt), event.Method,
		event.Path, event.Provider, event.Model, event.StatusCode, event.Duration.Milliseconds(),
		boolInt(event.Streaming), event.Error, nullableBytes(event.RequestBody), nullableBytes(event.ResponseBody),
		nullableBytes(event.Metadata), event.AttemptCount, event.RetryCount, event.InputTokens,
		event.OutputTokens, event.CacheReadInputTokens, event.CacheCreationInputTokens, event.ChunkCount,
		event.StopReason, event.CompletionState, event.FailureKind, boolInt(event.Canceled),
		boolInt(event.Truncated), event.APIKeyLabel, event.TaskHash, event.ClaudeRequestBytes,
		event.UpstreamRequestBytes, event.UpstreamResponseBytes, event.ClaudeResponseBytes,
		event.MessageCount, event.ToolCount)
	if err != nil {
		return fmt.Errorf("insert diagnostics event %q: %w", event.RequestID, err)
	}
	return nil
}

func (store *Store) upsertEvent(ctx context.Context, executor sqlExecutor, event Event) error {
	_, err := executor.ExecContext(ctx, `
		INSERT INTO diagnostics_events (
			request_id, created_at, updated_at, method, path, provider, model,
			status_code, duration_ms, streaming, error, request_body, response_body, metadata,
			attempt_count, retry_count, input_tokens, output_tokens, cache_read_input_tokens,
			cache_creation_input_tokens, chunk_count, stop_reason, completion_state, failure_kind,
			canceled, truncated, api_key_label, task_hash, claude_request_bytes, upstream_request_bytes,
			upstream_response_bytes, claude_response_bytes, message_count, tool_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(request_id) DO UPDATE SET
			created_at=excluded.created_at, updated_at=excluded.updated_at, method=excluded.method,
			path=excluded.path, provider=excluded.provider, model=excluded.model,
			status_code=excluded.status_code, duration_ms=excluded.duration_ms, streaming=excluded.streaming,
			error=excluded.error, request_body=excluded.request_body, response_body=excluded.response_body,
			metadata=excluded.metadata, attempt_count=excluded.attempt_count, retry_count=excluded.retry_count,
			input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
			cache_read_input_tokens=excluded.cache_read_input_tokens,
			cache_creation_input_tokens=excluded.cache_creation_input_tokens, chunk_count=excluded.chunk_count,
			stop_reason=excluded.stop_reason, completion_state=excluded.completion_state,
			failure_kind=excluded.failure_kind, canceled=excluded.canceled, truncated=excluded.truncated,
			api_key_label=excluded.api_key_label, task_hash=excluded.task_hash,
			claude_request_bytes=excluded.claude_request_bytes,
			upstream_request_bytes=excluded.upstream_request_bytes,
			upstream_response_bytes=excluded.upstream_response_bytes,
			claude_response_bytes=excluded.claude_response_bytes,
			message_count=excluded.message_count, tool_count=excluded.tool_count`,
		event.RequestID, toMillis(event.CreatedAt), toMillis(event.UpdatedAt), event.Method,
		event.Path, event.Provider, event.Model, event.StatusCode, event.Duration.Milliseconds(),
		boolInt(event.Streaming), event.Error, nullableBytes(event.RequestBody), nullableBytes(event.ResponseBody),
		nullableBytes(event.Metadata), event.AttemptCount, event.RetryCount, event.InputTokens,
		event.OutputTokens, event.CacheReadInputTokens, event.CacheCreationInputTokens, event.ChunkCount,
		event.StopReason, event.CompletionState, event.FailureKind, boolInt(event.Canceled),
		boolInt(event.Truncated), event.APIKeyLabel, event.TaskHash, event.ClaudeRequestBytes,
		event.UpstreamRequestBytes, event.UpstreamResponseBytes, event.ClaudeResponseBytes,
		event.MessageCount, event.ToolCount)
	if err != nil {
		return fmt.Errorf("upsert diagnostics event %q: %w", event.RequestID, err)
	}
	return nil
}

func (store *Store) normalizeSnapshot(snapshot ContentSnapshot) (ContentSnapshot, error) {
	if strings.TrimSpace(snapshot.RequestID) == "" {
		return ContentSnapshot{}, errors.New("diagnostics content request ID is required")
	}
	if snapshot.AttemptNumber < 0 {
		return ContentSnapshot{}, errors.New("diagnostics content attempt number must not be negative")
	}
	if !validContentBoundary(snapshot.Boundary) {
		return ContentSnapshot{}, fmt.Errorf("unsupported diagnostics content boundary %q", snapshot.Boundary)
	}
	if !json.Valid(snapshot.Content) {
		return ContentSnapshot{}, errors.New("diagnostics content must be valid JSON")
	}
	now := store.now().UTC()
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = now
	}
	if snapshot.ExpiresAt.IsZero() {
		snapshot.ExpiresAt = snapshot.CreatedAt.Add(store.contentRetention)
	}
	if snapshot.ExpiresAt.After(snapshot.CreatedAt.Add(store.contentRetention)) {
		snapshot.ExpiresAt = snapshot.CreatedAt.Add(store.contentRetention)
	}
	if snapshot.CaptureMode == "" {
		snapshot.CaptureMode = ContentCaptureFull
	}
	if snapshot.CaptureMode != ContentCaptureFull && snapshot.CaptureMode != ContentCaptureSummary {
		return ContentSnapshot{}, fmt.Errorf("unsupported diagnostics content capture mode %q", snapshot.CaptureMode)
	}
	return snapshot, nil
}

func (store *Store) insertContent(ctx context.Context, executor sqlExecutor, snapshot ContentSnapshot) error {
	_, err := executor.ExecContext(ctx, `
		INSERT INTO diagnostics_content (request_id, attempt_number, boundary, created_at, expires_at, capture_mode, content)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(request_id, attempt_number, boundary) DO UPDATE SET
			created_at=excluded.created_at, expires_at=excluded.expires_at, capture_mode=excluded.capture_mode, content=excluded.content`,
		snapshot.RequestID, snapshot.AttemptNumber, snapshot.Boundary, toMillis(snapshot.CreatedAt),
		toMillis(snapshot.ExpiresAt), snapshot.CaptureMode, []byte(snapshot.Content))
	if err != nil {
		return fmt.Errorf("insert diagnostics content request %q attempt %d boundary %q: %w", snapshot.RequestID, snapshot.AttemptNumber, snapshot.Boundary, err)
	}
	return nil
}

func (store *Store) Insert(ctx context.Context, event Event) error {
	event, err := store.normalizeEvent(event)
	if err != nil {
		return err
	}
	return store.insertEvent(ctx, store.db, event)
}

func (store *Store) insertBundle(ctx context.Context, event Event, snapshots []ContentSnapshot) error {
	event, err := store.normalizeEvent(event)
	if err != nil {
		return err
	}
	normalized := make([]ContentSnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.RequestID != event.RequestID {
			return fmt.Errorf("diagnostics content request ID %q does not match event request ID %q", snapshot.RequestID, event.RequestID)
		}
		snapshot, err = store.normalizeSnapshot(snapshot)
		if err != nil {
			return err
		}
		normalized = append(normalized, snapshot)
	}
	if len(normalized) == 0 {
		return store.upsertEvent(ctx, store.db, event)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin diagnostics persistence transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
		return fmt.Errorf("defer diagnostics content foreign keys: %w", err)
	}
	if err := store.upsertEvent(ctx, tx, event); err != nil {
		return err
	}
	for _, snapshot := range normalized {
		if err := store.insertContent(ctx, tx, snapshot); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit diagnostics persistence transaction: %w", err)
	}
	committed = true
	return nil
}

func (store *Store) InsertContent(ctx context.Context, snapshot ContentSnapshot) error {
	if !store.captureContent {
		return nil
	}
	snapshot, err := store.normalizeSnapshot(snapshot)
	if err != nil {
		return err
	}
	return store.insertContent(ctx, store.db, snapshot)
}

func (store *Store) Content(ctx context.Context, query ContentQuery) ([]ContentSnapshot, error) {
	if strings.TrimSpace(query.RequestID) == "" {
		return nil, errors.New("diagnostics content request ID is required")
	}
	if query.AttemptNumber != nil && *query.AttemptNumber < 0 {
		return nil, errors.New("diagnostics content attempt number must not be negative")
	}
	if query.Boundary != "" && !validContentBoundary(query.Boundary) {
		return nil, fmt.Errorf("unsupported diagnostics content boundary %q", query.Boundary)
	}
	statement := `SELECT request_id, attempt_number, boundary, created_at, expires_at, capture_mode, content
		FROM diagnostics_content WHERE request_id = ? AND expires_at > ?`
	args := []any{query.RequestID, toMillis(store.now().UTC())}
	if query.AttemptNumber != nil {
		statement += " AND attempt_number = ?"
		args = append(args, *query.AttemptNumber)
	}
	if query.Boundary != "" {
		statement += " AND boundary = ?"
		args = append(args, query.Boundary)
	}
	statement += " ORDER BY attempt_number ASC, boundary ASC"
	rows, err := store.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query diagnostics content: %w", err)
	}
	defer rows.Close()
	var snapshots []ContentSnapshot
	for rows.Next() {
		var snapshot ContentSnapshot
		var createdAt, expiresAt int64
		var content []byte
		if err := rows.Scan(&snapshot.RequestID, &snapshot.AttemptNumber, &snapshot.Boundary, &createdAt, &expiresAt, &snapshot.CaptureMode, &content); err != nil {
			return nil, fmt.Errorf("scan diagnostics content: %w", err)
		}
		snapshot.CreatedAt, snapshot.ExpiresAt = fromMillis(createdAt), fromMillis(expiresAt)
		snapshot.Content = cloneRaw(content)
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate diagnostics content: %w", err)
	}
	return snapshots, nil
}

func RepresentJSONContent(body []byte, options ContentRepresentationOptions) (json.RawMessage, ContentCaptureMode, error) {
	if !json.Valid(body) {
		return nil, "", errors.New("diagnostics content must be valid JSON")
	}
	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxContentBytes
	}
	if len(body) <= maxBytes {
		return cloneRaw(body), ContentCaptureFull, nil
	}
	previewBytes := options.PreviewBytes
	if previewBytes <= 0 {
		previewBytes = DefaultContentPreviewBytes
	}
	previewBytes = minInt(previewBytes, len(body)/2)
	head, tail := utf8Head(body, previewBytes), utf8Tail(body, previewBytes)
	totalChars := utf8.RuneCount(body)
	displayedChars := utf8.RuneCountInString(head) + utf8.RuneCountInString(tail)
	summary := PartialJSONSummary{
		Partial: true, Reason: options.Reason, SourceBytes: options.SourceBytes,
		SourceTruncated: options.SourceTruncated, DisplayedChars: displayedChars,
		OmittedChars: maxInt(0, totalChars-displayedChars), Head: head, Tail: tail,
	}
	if summary.Reason == "" {
		summary.Reason = "content_limit"
	}
	if summary.SourceBytes == 0 {
		summary.SourceBytes = len(body)
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		return nil, "", fmt.Errorf("encode diagnostics content summary: %w", err)
	}
	return encoded, ContentCaptureSummary, nil
}

func captureSummary(capture ContentCapture) (json.RawMessage, ContentCaptureMode) {
	summary := PartialJSONSummary{Partial: true, Reason: capture.Reason, SourceBytes: capture.SourceBytes, SourceTruncated: capture.SourceTruncated}
	if summary.Reason == "" {
		summary.Reason = "capture_unavailable"
	}
	encoded, _ := json.Marshal(summary)
	return encoded, ContentCaptureSummary
}

func maxInt(left, right int) int { if left > right { return left }; return right }

func validContentBoundary(boundary ContentBoundary) bool {
	switch boundary {
	case ContentBoundaryClaudeRequest, ContentBoundaryUpstreamRequest, ContentBoundaryUpstreamResponse, ContentBoundaryClaudeResponse:
		return true
	default:
		return false
	}
}

func utf8Head(body []byte, limit int) string {
	if limit >= len(body) { return string(body) }
	end := limit
	for end > 0 && !utf8.Valid(body[:end]) { end-- }
	return string(body[:end])
}

func utf8Tail(body []byte, limit int) string {
	if limit >= len(body) { return string(body) }
	start := len(body) - limit
	for start < len(body) && !utf8.Valid(body[start:]) { start++ }
	return string(body[start:])
}

func minInt(left, right int) int { if left < right { return left }; return right }

const eventColumns = `request_id, created_at, updated_at, method, path, provider, model,
	status_code, duration_ms, streaming, error, request_body, response_body, metadata,
	attempt_count, retry_count, input_tokens, output_tokens, cache_read_input_tokens,
	cache_creation_input_tokens, chunk_count, stop_reason, completion_state, failure_kind,
	canceled, truncated, api_key_label, task_hash, claude_request_bytes, upstream_request_bytes,
	upstream_response_bytes, claude_response_bytes, message_count, tool_count`

const summaryColumns = `request_id, created_at, updated_at, method, path, provider, model,
	status_code, duration_ms, streaming, error, attempt_count, retry_count, input_tokens,
	output_tokens, cache_read_input_tokens, cache_creation_input_tokens, chunk_count, stop_reason,
	completion_state, failure_kind, canceled, truncated, api_key_label, task_hash, claude_request_bytes,
	upstream_request_bytes, upstream_response_bytes, claude_response_bytes, message_count, tool_count`

func (store *Store) Detail(ctx context.Context, requestID string) (Event, error) {
	return scanEvent(store.db.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM diagnostics_events WHERE request_id = ?", requestID))
}

func (store *Store) Query(ctx context.Context, query Query) ([]EventSummary, error) {
	where, args := buildWhere(query)
	order := "DESC"
	if query.Ascending {
		order = "ASC"
	}
	statement := "SELECT " + summaryColumns + " FROM diagnostics_events" + where + " ORDER BY created_at " + order + ", request_id " + order
	statement, args = addLimit(statement, args, query)
	rows, err := store.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query diagnostics events: %w", err)
	}
	defer rows.Close()
	var events []EventSummary
	for rows.Next() {
		event, err := scanSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("scan diagnostics summary: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate diagnostics events: %w", err)
	}
	return events, nil
}

func (store *Store) Count(ctx context.Context, query Query) (int, error) {
	where, args := buildWhere(query)
	var total int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM diagnostics_events"+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("count diagnostics events: %w", err)
	}
	return total, nil
}

func (store *Store) Export(ctx context.Context, query Query, yield func(Event) error) error {
	if yield == nil {
		return errors.New("diagnostics export callback is required")
	}
	where, args := buildWhere(query)
	order := "ASC"
	if !query.Ascending {
		order = "DESC"
	}
	statement := "SELECT " + eventColumns + " FROM diagnostics_events" + where + " ORDER BY created_at " + order + ", request_id " + order
	statement, args = addLimit(statement, args, query)
	rows, err := store.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("export diagnostics events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return err
		}
		if err := yield(event); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate diagnostics export: %w", err)
	}
	return nil
}

func (store *Store) Analytics(ctx context.Context, query Query) (Analytics, error) {
	query.Limit, query.Offset, query.Ascending = 0, 0, true
	query.Since, query.Until = normalizedBounds(query)
	events, err := store.Query(ctx, query)
	if err != nil {
		return Analytics{}, err
	}
	result := Analytics{
		Since:                query.Since,
		Until:                query.Until,
		ByModel:              make([]AnalyticsCount, 0),
		ByProvider:           make([]AnalyticsCount, 0),
		ByAPIKey:             make([]AnalyticsCount, 0),
		ByCompletionState:    make([]AnalyticsCount, 0),
		HourlyTimeline:       make([]AnalyticsHour, 0),
		ModelConsumption:     make([]ModelConsumption, 0),
		TaskGroups:           make([]TaskGroup, 0),
		TokenByteCorrelation: TokenByteCorrelation{Status: "insufficient_samples"},
	}
	models, providers, keys, states := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	hours, tasks := map[int64]*AnalyticsHour{}, map[string]int{}
	modelConsumption := map[string]*ModelConsumption{}
	modelLatencies := map[string]int64{}
	latencies := make([]int64, 0, len(events))
	inputTokens, requestBytes := make([]float64, 0, len(events)), make([]float64, 0, len(events))
	outputTokens, responseBytes := make([]float64, 0, len(events)), make([]float64, 0, len(events))
	totalTokensValues, totalBytesValues := make([]float64, 0, len(events)), make([]float64, 0, len(events))
	var latencyTotal int64
	for _, event := range events {
		result.Total++
		success := event.CompletionState == "completed" && event.StatusCode >= 200 && event.StatusCode < 400 && event.FailureKind == "" && !event.Canceled && !event.Truncated
		if success {
			result.Success++
		} else {
			result.Failure++
		}
		if event.Truncated { result.Truncated++ }
		if event.Canceled { result.Canceled++ }
		if event.Streaming { result.Streaming++ }
		result.Retries += event.RetryCount
		result.Attempts += event.AttemptCount
		result.InputTokens += int64(event.InputTokens)
		result.OutputTokens += int64(event.OutputTokens)
		result.CacheReadInputTokens += int64(event.CacheReadInputTokens)
		result.CacheCreationInputTokens += int64(event.CacheCreationInputTokens)
		result.ClaudeRequestBytes += int64(event.ClaudeRequestBytes)
		result.UpstreamRequestBytes += int64(event.UpstreamRequestBytes)
		result.UpstreamResponseBytes += int64(event.UpstreamResponseBytes)
		result.ClaudeResponseBytes += int64(event.ClaudeResponseBytes)
		latency := event.Duration.Milliseconds()
		latencies = append(latencies, latency)
		latencyTotal += latency
		modelName := emptyLabel(event.Model)
		models[modelName]++
		providers[emptyLabel(event.Provider)]++
		keys[emptyLabel(event.APIKeyLabel)]++
		states[emptyLabel(event.CompletionState)]++
		hour := event.CreatedAt.UTC().Truncate(time.Hour)
		bucket := hours[hour.Unix()]
		if bucket == nil { bucket = &AnalyticsHour{Hour: hour}; hours[hour.Unix()] = bucket }
		bucket.Total++
		bucket.InputTokens += int64(event.InputTokens)
		bucket.OutputTokens += int64(event.OutputTokens)
		bucket.ClaudeRequestBytes += int64(event.ClaudeRequestBytes)
		bucket.UpstreamRequestBytes += int64(event.UpstreamRequestBytes)
		bucket.UpstreamResponseBytes += int64(event.UpstreamResponseBytes)
		bucket.ClaudeResponseBytes += int64(event.ClaudeResponseBytes)
		if success { bucket.Success++ } else { bucket.Failure++ }
		if event.TaskHash == "" { result.TaskGroupingUnavailable++ } else { tasks[event.TaskHash]++ }
		consumption := modelConsumption[modelName]
		if consumption == nil { consumption = &ModelConsumption{Model: modelName}; modelConsumption[modelName] = consumption }
		consumption.Count++
		consumption.InputTokens += int64(event.InputTokens)
		consumption.OutputTokens += int64(event.OutputTokens)
		consumption.CacheReadInputTokens += int64(event.CacheReadInputTokens)
		consumption.CacheCreationInputTokens += int64(event.CacheCreationInputTokens)
		consumption.TotalTokens += int64(event.InputTokens + event.OutputTokens)
		consumption.ClaudeRequestBytes += int64(event.ClaudeRequestBytes)
		consumption.UpstreamResponseBytes += int64(event.UpstreamResponseBytes)
		modelLatencies[modelName] += latency
		inputTokens = append(inputTokens, float64(event.InputTokens))
		requestBytes = append(requestBytes, float64(event.ClaudeRequestBytes))
		outputTokens = append(outputTokens, float64(event.OutputTokens))
		responseBytes = append(responseBytes, float64(event.UpstreamResponseBytes+event.ClaudeResponseBytes))
		totalTokensValues = append(totalTokensValues, float64(event.InputTokens+event.OutputTokens))
		totalBytesValues = append(totalBytesValues, float64(event.ClaudeRequestBytes+event.UpstreamRequestBytes+event.UpstreamResponseBytes+event.ClaudeResponseBytes))
	}
	if result.Total > 0 {
		result.AverageAttempts = float64(result.Attempts) / float64(result.Total)
		result.AverageLatencyMS = float64(latencyTotal) / float64(result.Total)
		result.AverageClaudeRequestBytes = float64(result.ClaudeRequestBytes) / float64(result.Total)
		result.AverageTotalTokens = float64(result.InputTokens+result.OutputTokens) / float64(result.Total)
		result.BytesPerInputToken = ratio(result.ClaudeRequestBytes, result.InputTokens)
		result.BytesPerOutputToken = ratio(result.UpstreamResponseBytes+result.ClaudeResponseBytes, result.OutputTokens)
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		result.P95LatencyMS = latencies[int(math.Ceil(float64(len(latencies))*0.95))-1]
	}
	result.TokenByteCorrelation = tokenByteCorrelation(inputTokens, requestBytes, outputTokens, responseBytes, totalTokensValues, totalBytesValues)
	result.ByModel = sortedCounts(models)
	result.ByProvider = sortedCounts(providers)
	result.ByAPIKey = sortedCounts(keys)
	result.ByCompletionState = sortedCounts(states)
	for _, bucket := range hours { result.HourlyTimeline = append(result.HourlyTimeline, *bucket) }
	sort.Slice(result.HourlyTimeline, func(i, j int) bool { return result.HourlyTimeline[i].Hour.Before(result.HourlyTimeline[j].Hour) })
	for _, consumption := range modelConsumption {
		if consumption.Count > 0 {
			consumption.AverageLatencyMS = float64(modelLatencies[consumption.Model]) / float64(consumption.Count)
			consumption.BytesPerToken = ratio(consumption.ClaudeRequestBytes+consumption.UpstreamResponseBytes, consumption.TotalTokens)
		}
		result.ModelConsumption = append(result.ModelConsumption, *consumption)
	}
	sort.Slice(result.ModelConsumption, func(i, j int) bool { if result.ModelConsumption[i].TotalTokens == result.ModelConsumption[j].TotalTokens { return result.ModelConsumption[i].Model < result.ModelConsumption[j].Model }; return result.ModelConsumption[i].TotalTokens > result.ModelConsumption[j].TotalTokens })
	for taskHash, count := range tasks { result.TaskGroups = append(result.TaskGroups, TaskGroup{TaskHash: taskHash, Count: count}) }
	sort.Slice(result.TaskGroups, func(i, j int) bool { if result.TaskGroups[i].Count == result.TaskGroups[j].Count { return result.TaskGroups[i].TaskHash < result.TaskGroups[j].TaskHash }; return result.TaskGroups[i].Count > result.TaskGroups[j].Count })
	return result, nil
}

func ratio(numerator, denominator int64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func tokenByteCorrelation(inputTokens, requestBytes, outputTokens, responseBytes, totalTokens, totalBytes []float64) TokenByteCorrelation {
	result := TokenByteCorrelation{SampleCount: len(totalTokens), Status: "insufficient_samples"}
	if len(totalTokens) < 2 {
		return result
	}
	input, inputOK := pearson(inputTokens, requestBytes)
	output, outputOK := pearson(outputTokens, responseBytes)
	total, totalOK := pearson(totalTokens, totalBytes)
	if !inputOK || !outputOK || !totalOK {
		result.Status = "zero_variance"
		return result
	}
	result.InputTokenVsRequestBytes = input
	result.OutputTokenVsResponseBytes = output
	result.TotalTokenVsTotalBytes = total
	result.Status = "ok"
	return result
}

func pearson(left, right []float64) (float64, bool) {
	if len(left) != len(right) || len(left) < 2 {
		return 0, false
	}
	var leftSum, rightSum float64
	for index := range left {
		leftSum += left[index]
		rightSum += right[index]
	}
	leftMean, rightMean := leftSum/float64(len(left)), rightSum/float64(len(right))
	var numerator, leftVariance, rightVariance float64
	for index := range left {
		leftDelta, rightDelta := left[index]-leftMean, right[index]-rightMean
		numerator += leftDelta * rightDelta
		leftVariance += leftDelta * leftDelta
		rightVariance += rightDelta * rightDelta
	}
	if leftVariance == 0 || rightVariance == 0 {
		return 0, false
	}
	return numerator / math.Sqrt(leftVariance*rightVariance), true
}

func normalizedBounds(query Query) (time.Time, time.Time) {
	return query.Since.UTC(), query.Until.UTC()
}

func (store *Store) Delete(ctx context.Context, requestID string) (bool, error) {
	result, err := store.db.ExecContext(ctx, "DELETE FROM diagnostics_events WHERE request_id = ?", requestID)
	if err != nil { return false, fmt.Errorf("delete diagnostics event %q: %w", requestID, err) }
	count, err := result.RowsAffected()
	if err != nil { return false, fmt.Errorf("count deleted diagnostics events: %w", err) }
	return count > 0, nil
}

func (store *Store) Clear(ctx context.Context) (int64, error) {
	result, err := store.db.ExecContext(ctx, "DELETE FROM diagnostics_events")
	if err != nil { return 0, fmt.Errorf("clear diagnostics events: %w", err) }
	count, err := result.RowsAffected()
	if err != nil { return 0, fmt.Errorf("count cleared diagnostics events: %w", err) }
	return count, nil
}

func (store *Store) Cleanup(ctx context.Context) (int64, error) {
	return store.CleanupBefore(ctx, store.now().UTC().Add(-store.retention))
}

func (store *Store) CleanupBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if _, err := store.db.ExecContext(ctx, "DELETE FROM diagnostics_content WHERE expires_at <= ?", toMillis(store.now().UTC())); err != nil {
		return 0, fmt.Errorf("clean up diagnostics content: %w", err)
	}
	result, err := store.db.ExecContext(ctx, "DELETE FROM diagnostics_events WHERE created_at < ?", toMillis(cutoff))
	if err != nil { return 0, fmt.Errorf("clean up diagnostics events: %w", err) }
	count, err := result.RowsAffected()
	if err != nil { return 0, fmt.Errorf("count cleaned diagnostics events: %w", err) }
	return count, nil
}

func buildWhere(query Query) (string, []any) {
	var clauses []string
	var args []any
	if query.RequestID != "" { clauses = append(clauses, "request_id = ?"); args = append(args, query.RequestID) }
	if query.Model != "" { clauses = append(clauses, "model = ?"); args = append(args, query.Model) }
	if query.Provider != "" { clauses = append(clauses, "provider = ?"); args = append(args, query.Provider) }
	if query.CompletionState != "" { clauses = append(clauses, "completion_state = ?"); args = append(args, query.CompletionState) }
	if query.Streaming != nil { clauses = append(clauses, "streaming = ?"); args = append(args, boolInt(*query.Streaming)) }
	if !query.Since.IsZero() { clauses = append(clauses, "created_at >= ?"); args = append(args, toMillis(query.Since)) }
	if !query.Until.IsZero() { clauses = append(clauses, "created_at <= ?"); args = append(args, toMillis(query.Until)) }
	if len(clauses) == 0 { return "", args }
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func addLimit(statement string, args []any, query Query) (string, []any) {
	if query.Limit > 0 {
		statement += " LIMIT ?"; args = append(args, query.Limit)
		if query.Offset > 0 { statement += " OFFSET ?"; args = append(args, query.Offset) }
	} else if query.Offset > 0 { statement += " LIMIT -1 OFFSET ?"; args = append(args, query.Offset) }
	return statement, args
}

type scanner interface { Scan(dest ...any) error }

func scanEvent(source scanner) (Event, error) {
	var event Event
	var createdAt, updatedAt, durationMS int64
	var streaming, canceled, truncated int
	var requestBody, responseBody, metadata []byte
	err := source.Scan(&event.RequestID, &createdAt, &updatedAt, &event.Method, &event.Path, &event.Provider,
		&event.Model, &event.StatusCode, &durationMS, &streaming, &event.Error, &requestBody, &responseBody,
		&metadata, &event.AttemptCount, &event.RetryCount, &event.InputTokens, &event.OutputTokens,
		&event.CacheReadInputTokens, &event.CacheCreationInputTokens, &event.ChunkCount, &event.StopReason,
		&event.CompletionState, &event.FailureKind, &canceled, &truncated, &event.APIKeyLabel, &event.TaskHash,
		&event.ClaudeRequestBytes, &event.UpstreamRequestBytes, &event.UpstreamResponseBytes,
		&event.ClaudeResponseBytes, &event.MessageCount, &event.ToolCount)
	if err != nil { return Event{}, err }
	event.CreatedAt, event.UpdatedAt = fromMillis(createdAt), fromMillis(updatedAt)
	event.Duration, event.Streaming = time.Duration(durationMS)*time.Millisecond, streaming != 0
	event.Canceled, event.Truncated = canceled != 0, truncated != 0
	event.RequestBody, event.ResponseBody, event.Metadata = cloneRaw(requestBody), cloneRaw(responseBody), cloneRaw(metadata)
	return event, nil
}

func scanSummary(source scanner) (EventSummary, error) {
	var event EventSummary
	var createdAt, updatedAt, durationMS int64
	var streaming, canceled, truncated int
	err := source.Scan(&event.RequestID, &createdAt, &updatedAt, &event.Method, &event.Path, &event.Provider,
		&event.Model, &event.StatusCode, &durationMS, &streaming, &event.Error, &event.AttemptCount,
		&event.RetryCount, &event.InputTokens, &event.OutputTokens, &event.CacheReadInputTokens,
		&event.CacheCreationInputTokens, &event.ChunkCount, &event.StopReason, &event.CompletionState,
		&event.FailureKind, &canceled, &truncated, &event.APIKeyLabel, &event.TaskHash,
		&event.ClaudeRequestBytes, &event.UpstreamRequestBytes, &event.UpstreamResponseBytes,
		&event.ClaudeResponseBytes, &event.MessageCount, &event.ToolCount)
	if err != nil { return EventSummary{}, err }
	event.CreatedAt, event.UpdatedAt = fromMillis(createdAt), fromMillis(updatedAt)
	event.Duration, event.Streaming = time.Duration(durationMS)*time.Millisecond, streaming != 0
	event.Canceled, event.Truncated = canceled != 0, truncated != 0
	return event, nil
}

func sortedCounts(values map[string]int) []AnalyticsCount {
	result := make([]AnalyticsCount, 0, len(values))
	for name, count := range values { result = append(result, AnalyticsCount{Name: name, Count: count}) }
	sort.Slice(result, func(i, j int) bool { if result[i].Count == result[j].Count { return result[i].Name < result[j].Name }; return result[i].Count > result[j].Count })
	return result
}
func emptyLabel(value string) string { if value == "" { return "unavailable" }; return value }
func nullableBytes(value json.RawMessage) any { if len(value) == 0 { return nil }; return []byte(value) }
func cloneRaw(value []byte) json.RawMessage { if len(value) == 0 { return nil }; return append(json.RawMessage(nil), value...) }
func boolInt(value bool) int { if value { return 1 }; return 0 }
func toMillis(value time.Time) int64 { return value.UTC().UnixMilli() }
func fromMillis(value int64) time.Time { return time.UnixMilli(value).UTC() }
