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
	"time"

	_ "modernc.org/sqlite"
)

const DefaultRetention = 72 * time.Hour

type StoreOptions struct {
	Retention   time.Duration
	BusyTimeout time.Duration
	Now         func() time.Time
}

type Store struct {
	db        *sql.DB
	retention time.Duration
	now       func() time.Time
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
	TaskHash        string          `json:"task_hash,omitempty"`
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
	TaskHash        string        `json:"task_hash,omitempty"`
}

type Query struct {
	RequestID string
	Model     string
	Provider  string
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
	Ascending bool
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
	AverageLatencyMS        float64          `json:"average_latency_ms"`
	P95LatencyMS            int64            `json:"p95_latency_ms"`
	ByModel                 []AnalyticsCount `json:"by_model"`
	ByProvider              []AnalyticsCount `json:"by_provider"`
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
	Hour    time.Time `json:"hour"`
	Total   int       `json:"total"`
	Success int       `json:"success"`
	Failure int       `json:"failure"`
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
	store := &Store{db: db, retention: options.Retention, now: options.Now}
	if store.retention <= 0 {
		store.retention = DefaultRetention
	}
	if store.now == nil {
		store.now = time.Now
	}
	if err := store.initialize(context.Background(), busyTimeout, path != ":memory:"); err != nil {
		db.Close()
		return nil, err
	}
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
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read diagnostics schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("diagnostics schema version %d is newer than supported version %d", version, schemaVersion)
	}
	switch version {
	case 0:
		if _, err := store.db.ExecContext(ctx, createSchemaV2SQL); err != nil {
			return fmt.Errorf("create diagnostics schema: %w", err)
		}
	case 1:
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin diagnostics schema migration: %w", err)
		}
		for _, statement := range migrateV1ToV2Statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate diagnostics schema v1 to v2: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit diagnostics schema migration: %w", err)
		}
	}
	if err := store.validateAndRepairSchemaV2(ctx); err != nil {
		return err
	}
	return nil
}

var requiredSchemaV2Columns = []string{
	"request_id", "created_at", "updated_at", "method", "path", "provider", "model",
	"status_code", "duration_ms", "streaming", "error", "request_body", "response_body", "metadata",
	"attempt_count", "retry_count", "input_tokens", "output_tokens", "cache_read_input_tokens",
	"cache_creation_input_tokens", "chunk_count", "stop_reason", "completion_state", "failure_kind",
	"canceled", "truncated", "task_hash",
}

var requiredSchemaV2Indexes = map[string]struct {
	columns   []string
	statement string
}{
	"idx_diagnostics_events_created_at": {columns: []string{"created_at", "request_id"}, statement: `CREATE INDEX idx_diagnostics_events_created_at ON diagnostics_events(created_at DESC, request_id DESC)`},
	"idx_diagnostics_events_model":      {columns: []string{"model", "created_at"}, statement: `CREATE INDEX idx_diagnostics_events_model ON diagnostics_events(model, created_at DESC)`},
	"idx_diagnostics_events_task_hash":  {columns: []string{"task_hash", "created_at"}, statement: `CREATE INDEX idx_diagnostics_events_task_hash ON diagnostics_events(task_hash, created_at DESC)`},
}

func (store *Store) validateAndRepairSchemaV2(ctx context.Context) error {
	rows, err := store.db.QueryContext(ctx, "PRAGMA table_info(diagnostics_events)")
	if err != nil {
		return fmt.Errorf("inspect diagnostics schema v2 columns: %w", err)
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan diagnostics schema v2 columns: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate diagnostics schema v2 columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close diagnostics schema v2 column inspection: %w", err)
	}
	var missing []string
	for _, name := range requiredSchemaV2Columns {
		if !columns[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("diagnostics schema v2 is incompatible: missing required columns: %s", strings.Join(missing, ", "))
	}

	for name, required := range requiredSchemaV2Indexes {
		var found string
		err := store.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := store.db.ExecContext(ctx, required.statement); err != nil {
				return fmt.Errorf("repair diagnostics schema v2 index %s: %w", name, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect diagnostics schema v2 index %s: %w", name, err)
		}
		indexRows, err := store.db.QueryContext(ctx, "PRAGMA index_info("+name+")")
		if err != nil {
			return fmt.Errorf("inspect diagnostics schema v2 index %s columns: %w", name, err)
		}
		var actual []string
		for indexRows.Next() {
			var sequence, cid int
			var column string
			if err := indexRows.Scan(&sequence, &cid, &column); err != nil {
				_ = indexRows.Close()
				return fmt.Errorf("scan diagnostics schema v2 index %s columns: %w", name, err)
			}
			actual = append(actual, column)
		}
		if err := indexRows.Err(); err != nil {
			_ = indexRows.Close()
			return fmt.Errorf("iterate diagnostics schema v2 index %s columns: %w", name, err)
		}
		if err := indexRows.Close(); err != nil {
			return fmt.Errorf("close diagnostics schema v2 index %s inspection: %w", name, err)
		}
		if !sameStrings(actual, required.columns) {
			return fmt.Errorf("diagnostics schema v2 is incompatible: index %s has columns %v, want %v", name, actual, required.columns)
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
	return store.db.Close()
}

func (store *Store) Insert(ctx context.Context, event Event) error {
	if strings.TrimSpace(event.RequestID) == "" {
		return errors.New("diagnostics request ID is required")
	}
	now := store.now().UTC()
	if event.CreatedAt.IsZero() {
		event.CreatedAt = now
	}
	if event.UpdatedAt.IsZero() {
		event.UpdatedAt = event.CreatedAt
	}
	_, err := store.db.ExecContext(ctx, `
	INSERT INTO diagnostics_events (
		request_id, created_at, updated_at, method, path, provider, model,
		status_code, duration_ms, streaming, error, request_body, response_body, metadata,
		attempt_count, retry_count, input_tokens, output_tokens, cache_read_input_tokens,
		cache_creation_input_tokens, chunk_count, stop_reason, completion_state, failure_kind,
		canceled, truncated, task_hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.RequestID, toMillis(event.CreatedAt), toMillis(event.UpdatedAt), event.Method,
		event.Path, event.Provider, event.Model, event.StatusCode, event.Duration.Milliseconds(),
		boolInt(event.Streaming), event.Error, nullableBytes(event.RequestBody), nullableBytes(event.ResponseBody),
		nullableBytes(event.Metadata), event.AttemptCount, event.RetryCount, event.InputTokens,
		event.OutputTokens, event.CacheReadInputTokens, event.CacheCreationInputTokens, event.ChunkCount,
		event.StopReason, event.CompletionState, event.FailureKind, boolInt(event.Canceled),
		boolInt(event.Truncated), event.TaskHash)
	if err != nil {
		return fmt.Errorf("insert diagnostics event %q: %w", event.RequestID, err)
	}
	return nil
}

const eventColumns = `request_id, created_at, updated_at, method, path, provider, model,
	status_code, duration_ms, streaming, error, request_body, response_body, metadata,
	attempt_count, retry_count, input_tokens, output_tokens, cache_read_input_tokens,
	cache_creation_input_tokens, chunk_count, stop_reason, completion_state, failure_kind,
	canceled, truncated, task_hash`

const summaryColumns = `request_id, created_at, updated_at, method, path, provider, model,
	status_code, duration_ms, streaming, error, attempt_count, retry_count, input_tokens,
	output_tokens, cache_read_input_tokens, cache_creation_input_tokens, chunk_count, stop_reason,
	completion_state, failure_kind, canceled, truncated, task_hash`

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
		Since:             query.Since,
		Until:             query.Until,
		ByModel:           make([]AnalyticsCount, 0),
		ByProvider:        make([]AnalyticsCount, 0),
		ByCompletionState: make([]AnalyticsCount, 0),
		HourlyTimeline:    make([]AnalyticsHour, 0),
		TaskGroups:        make([]TaskGroup, 0),
	}
	models, providers, states := map[string]int{}, map[string]int{}, map[string]int{}
	hours, tasks := map[int64]*AnalyticsHour{}, map[string]int{}
	latencies := make([]int64, 0, len(events))
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
		latency := event.Duration.Milliseconds()
		latencies = append(latencies, latency)
		latencyTotal += latency
		models[emptyLabel(event.Model)]++
		providers[emptyLabel(event.Provider)]++
		states[emptyLabel(event.CompletionState)]++
		hour := event.CreatedAt.UTC().Truncate(time.Hour)
		bucket := hours[hour.Unix()]
		if bucket == nil { bucket = &AnalyticsHour{Hour: hour}; hours[hour.Unix()] = bucket }
		bucket.Total++
		if success { bucket.Success++ } else { bucket.Failure++ }
		if event.TaskHash == "" { result.TaskGroupingUnavailable++ } else { tasks[event.TaskHash]++ }
	}
	if result.Total > 0 {
		result.AverageAttempts = float64(result.Attempts) / float64(result.Total)
		result.AverageLatencyMS = float64(latencyTotal) / float64(result.Total)
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		result.P95LatencyMS = latencies[int(math.Ceil(float64(len(latencies))*0.95))-1]
	}
	result.ByModel = sortedCounts(models)
	result.ByProvider = sortedCounts(providers)
	result.ByCompletionState = sortedCounts(states)
	for _, bucket := range hours { result.HourlyTimeline = append(result.HourlyTimeline, *bucket) }
	sort.Slice(result.HourlyTimeline, func(i, j int) bool { return result.HourlyTimeline[i].Hour.Before(result.HourlyTimeline[j].Hour) })
	for taskHash, count := range tasks { result.TaskGroups = append(result.TaskGroups, TaskGroup{TaskHash: taskHash, Count: count}) }
	sort.Slice(result.TaskGroups, func(i, j int) bool { if result.TaskGroups[i].Count == result.TaskGroups[j].Count { return result.TaskGroups[i].TaskHash < result.TaskGroups[j].TaskHash }; return result.TaskGroups[i].Count > result.TaskGroups[j].Count })
	return result, nil
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

func (store *Store) Cleanup(ctx context.Context) (int64, error) { return store.CleanupBefore(ctx, store.now().UTC().Add(-store.retention)) }
func (store *Store) CleanupBefore(ctx context.Context, cutoff time.Time) (int64, error) {
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
		&event.CompletionState, &event.FailureKind, &canceled, &truncated, &event.TaskHash)
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
		&event.FailureKind, &canceled, &truncated, &event.TaskHash)
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
