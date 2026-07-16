package diagnostics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const DefaultRetention = 72 * time.Hour

type StoreOptions struct {
	Retention  time.Duration
	BusyTimeout time.Duration
	Now        func() time.Time
}

type Store struct {
	db        *sql.DB
	retention time.Duration
	now       func() time.Time
}

type Event struct {
	RequestID    string          `json:"request_id"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	Method       string          `json:"method,omitempty"`
	Path         string          `json:"path,omitempty"`
	Provider     string          `json:"provider,omitempty"`
	Model        string          `json:"model,omitempty"`
	StatusCode   int             `json:"status_code,omitempty"`
	Duration     time.Duration   `json:"duration,omitempty"`
	Streaming    bool            `json:"streaming"`
	Error        string          `json:"error,omitempty"`
	RequestBody  json.RawMessage `json:"request_body,omitempty"`
	ResponseBody json.RawMessage `json:"response_body,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

type EventSummary struct {
	RequestID  string        `json:"request_id"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	Method     string        `json:"method,omitempty"`
	Path       string        `json:"path,omitempty"`
	Provider   string        `json:"provider,omitempty"`
	Model      string        `json:"model,omitempty"`
	StatusCode int           `json:"status_code,omitempty"`
	Duration   time.Duration `json:"duration,omitempty"`
	Streaming  bool          `json:"streaming"`
	Error      string        `json:"error,omitempty"`
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
	dsn := sqliteDSN(path, busyTimeout)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open diagnostics database: %w", err)
	}
	// A single connection avoids per-connection pragma drift and serializes writes.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db, retention: options.Retention, now: options.Now}
	if store.retention <= 0 {
		store.retention = DefaultRetention
	}
	if store.now == nil {
		store.now = time.Now
	}
	if err := store.initialize(context.Background(), busyTimeout); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func sqliteDSN(path string, busyTimeout time.Duration) string {
	milliseconds := busyTimeout.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	if path == ":memory:" {
		return fmt.Sprintf("file:diagnostics?mode=memory&cache=shared&_pragma=busy_timeout%%28%d%%29&_pragma=journal_mode%%28WAL%%29", milliseconds)
	}
	fileURL := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	return fileURL.String() + fmt.Sprintf("?_pragma=busy_timeout%%28%d%%29&_pragma=journal_mode%%28WAL%%29", milliseconds)
}

func (store *Store) initialize(ctx context.Context, busyTimeout time.Duration) error {
	if err := store.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping diagnostics database: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set diagnostics busy timeout: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("enable diagnostics WAL mode: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, createSchemaSQL); err != nil {
		return fmt.Errorf("create diagnostics schema: %w", err)
	}
	return nil
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
	status_code, duration_ms, streaming, error, request_body, response_body, metadata
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.RequestID, toMillis(event.CreatedAt), toMillis(event.UpdatedAt), event.Method,
		event.Path, event.Provider, event.Model, event.StatusCode, event.Duration.Milliseconds(),
		boolInt(event.Streaming), event.Error, nullableBytes(event.RequestBody),
		nullableBytes(event.ResponseBody), nullableBytes(event.Metadata))
	if err != nil {
		return fmt.Errorf("insert diagnostics event %q: %w", event.RequestID, err)
	}
	return nil
}

func (store *Store) Detail(ctx context.Context, requestID string) (Event, error) {
	row := store.db.QueryRowContext(ctx, `
SELECT request_id, created_at, updated_at, method, path, provider, model,
	status_code, duration_ms, streaming, error, request_body, response_body, metadata
FROM diagnostics_events WHERE request_id = ?`, requestID)
	return scanEvent(row)
}

func (store *Store) Query(ctx context.Context, query Query) ([]EventSummary, error) {
	where, args := buildWhere(query)
	order := "DESC"
	if query.Ascending {
		order = "ASC"
	}
	statement := `SELECT request_id, created_at, updated_at, method, path, provider, model,
	status_code, duration_ms, streaming, error FROM diagnostics_events` + where +
		" ORDER BY created_at " + order + ", request_id " + order
	if query.Limit > 0 {
		statement += " LIMIT ?"
		args = append(args, query.Limit)
		if query.Offset > 0 {
			statement += " OFFSET ?"
			args = append(args, query.Offset)
		}
	} else if query.Offset > 0 {
		statement += " LIMIT -1 OFFSET ?"
		args = append(args, query.Offset)
	}

	rows, err := store.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query diagnostics events: %w", err)
	}
	defer rows.Close()

	var events []EventSummary
	for rows.Next() {
		var event EventSummary
		var createdAt, updatedAt, durationMS int64
		var streaming int
		if err := rows.Scan(&event.RequestID, &createdAt, &updatedAt, &event.Method, &event.Path,
			&event.Provider, &event.Model, &event.StatusCode, &durationMS, &streaming, &event.Error); err != nil {
			return nil, fmt.Errorf("scan diagnostics summary: %w", err)
		}
		event.CreatedAt = fromMillis(createdAt)
		event.UpdatedAt = fromMillis(updatedAt)
		event.Duration = time.Duration(durationMS) * time.Millisecond
		event.Streaming = streaming != 0
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
	statement := `SELECT request_id, created_at, updated_at, method, path, provider, model,
	status_code, duration_ms, streaming, error, request_body, response_body, metadata
FROM diagnostics_events` + where + " ORDER BY created_at " + order + ", request_id " + order
	if query.Limit > 0 {
		statement += " LIMIT ?"
		args = append(args, query.Limit)
		if query.Offset > 0 {
			statement += " OFFSET ?"
			args = append(args, query.Offset)
		}
	} else if query.Offset > 0 {
		statement += " LIMIT -1 OFFSET ?"
		args = append(args, query.Offset)
	}
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

func (store *Store) Delete(ctx context.Context, requestID string) (bool, error) {
	result, err := store.db.ExecContext(ctx, "DELETE FROM diagnostics_events WHERE request_id = ?", requestID)
	if err != nil {
		return false, fmt.Errorf("delete diagnostics event %q: %w", requestID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count deleted diagnostics events: %w", err)
	}
	return count > 0, nil
}

func (store *Store) Clear(ctx context.Context) (int64, error) {
	result, err := store.db.ExecContext(ctx, "DELETE FROM diagnostics_events")
	if err != nil {
		return 0, fmt.Errorf("clear diagnostics events: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleared diagnostics events: %w", err)
	}
	return count, nil
}

func (store *Store) Cleanup(ctx context.Context) (int64, error) {
	return store.CleanupBefore(ctx, store.now().UTC().Add(-store.retention))
}

func (store *Store) CleanupBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := store.db.ExecContext(ctx, "DELETE FROM diagnostics_events WHERE created_at < ?", toMillis(cutoff))
	if err != nil {
		return 0, fmt.Errorf("clean up diagnostics events: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleaned diagnostics events: %w", err)
	}
	return count, nil
}

func buildWhere(query Query) (string, []any) {
	var clauses []string
	var args []any
	if query.RequestID != "" {
		clauses = append(clauses, "request_id = ?")
		args = append(args, query.RequestID)
	}
	if query.Model != "" {
		clauses = append(clauses, "model = ?")
		args = append(args, query.Model)
	}
	if query.Provider != "" {
		clauses = append(clauses, "provider = ?")
		args = append(args, query.Provider)
	}
	if !query.Since.IsZero() {
		clauses = append(clauses, "created_at >= ?")
		args = append(args, toMillis(query.Since))
	}
	if !query.Until.IsZero() {
		clauses = append(clauses, "created_at <= ?")
		args = append(args, toMillis(query.Until))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(source scanner) (Event, error) {
	var event Event
	var createdAt, updatedAt, durationMS int64
	var streaming int
	var requestBody, responseBody, metadata []byte
	if err := source.Scan(&event.RequestID, &createdAt, &updatedAt, &event.Method, &event.Path,
		&event.Provider, &event.Model, &event.StatusCode, &durationMS, &streaming, &event.Error,
		&requestBody, &responseBody, &metadata); err != nil {
		return Event{}, err
	}
	event.CreatedAt = fromMillis(createdAt)
	event.UpdatedAt = fromMillis(updatedAt)
	event.Duration = time.Duration(durationMS) * time.Millisecond
	event.Streaming = streaming != 0
	event.RequestBody = cloneRaw(requestBody)
	event.ResponseBody = cloneRaw(responseBody)
	event.Metadata = cloneRaw(metadata)
	return event, nil
}

func nullableBytes(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return []byte(value)
}

func cloneRaw(value []byte) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), value...)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func toMillis(value time.Time) int64 { return value.UTC().UnixMilli() }
func fromMillis(value int64) time.Time { return time.UnixMilli(value).UTC() }
