package promptarchive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/claude-code-proxy/proxy/pkg/models"
	_ "modernc.org/sqlite"
)

const (
	DefaultRetention = 168 * time.Hour
	DefaultMaxPayloadBytes = 5 * 1024 * 1024
)

type Options struct {
	Retention time.Duration
	BusyTimeout time.Duration
	Now func() time.Time
}

type Store struct {
	db *sql.DB
	retention time.Duration
	now func() time.Time
	jobs chan Record
	stop chan struct{}
	worker sync.WaitGroup
	dropped atomic.Uint64
	dbMu sync.Mutex
	closing bool
	lifecycleMu sync.Mutex
}

type Record struct {
	RequestID string `json:"request_id"`
	CreatedAt time.Time `json:"created_at"`
	Model string `json:"model"`
	MessageCount int `json:"message_count"`
	ToolCount int `json:"tool_count"`
	SystemBytes int `json:"system_bytes"`
	RawBytes int `json:"raw_bytes"`
	Summary string `json:"summary"`
	Truncated bool `json:"truncated"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Query struct {
	RequestID string
	Model string
	Keyword string
	Since time.Time
	Until time.Time
	Limit int
	Offset int
}

func Open(path string, options Options) (*Store, error) {
	if strings.TrimSpace(path) == "" { return nil, errors.New("prompt archive database path is required") }
	if path != ":memory:" {
		absolute, err := filepath.Abs(path); if err != nil { return nil, fmt.Errorf("resolve prompt archive database path: %w", err) }
		path = absolute
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil { return nil, fmt.Errorf("create prompt archive database directory: %w", err) }
	}
	db, err := sql.Open("sqlite", path); if err != nil { return nil, fmt.Errorf("open prompt archive database: %w", err) }
	db.SetMaxOpenConns(1); db.SetMaxIdleConns(1)
	store := &Store{db: db, retention: options.Retention, now: options.Now}
	if store.retention <= 0 { store.retention = DefaultRetention }
	if store.now == nil { store.now = time.Now }
	busy := options.BusyTimeout; if busy <= 0 { busy = 5 * time.Second }
	ctx, cancel := context.WithTimeout(context.Background(), busy+time.Second); defer cancel()
	if err := store.initialize(ctx, busy, path != ":memory:"); err != nil { db.Close(); return nil, err }
	store.startWriter()
	return store, nil
}

func (store *Store) initialize(ctx context.Context, busy time.Duration, wal bool) error {
	if err := store.db.PingContext(ctx); err != nil { return fmt.Errorf("ping prompt archive database: %w", err) }
	if _, err := store.db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busy.Milliseconds())); err != nil { return err }
	if wal { if _, err := store.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil { return err } }
	if _, err := store.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil { return err }
	_, err := store.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS prompt_archive (
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
);
CREATE TABLE IF NOT EXISTS prompt_archive_payloads (
	request_id TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	payload BLOB NOT NULL,
	FOREIGN KEY (request_id) REFERENCES prompt_archive(request_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_prompt_archive_created_at ON prompt_archive(created_at DESC, request_id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_archive_model ON prompt_archive(model, created_at DESC);
`)
	if err != nil { return fmt.Errorf("create prompt archive schema: %w", err) }
	_, err = store.db.ExecContext(ctx, "DELETE FROM prompt_archive WHERE created_at < ?", toMillis(store.now().UTC().Add(-store.retention)))
	if err != nil { return fmt.Errorf("clean prompt archive: %w", err) }
	return nil
}

func (store *Store) Close() error {
	if store == nil || store.db == nil { return nil }
	store.lifecycleMu.Lock(); if store.closing { store.lifecycleMu.Unlock(); return nil }; store.closing = true; close(store.stop); store.lifecycleMu.Unlock()
	store.worker.Wait()
	return store.db.Close()
}

func (store *Store) startWriter() {
	store.jobs = make(chan Record, 256); store.stop = make(chan struct{})
	store.worker.Add(1)
	go func(){ defer store.worker.Done(); for { select { case record := <-store.jobs: if err := store.Insert(context.Background(), record); err != nil { store.dropped.Add(1); fmt.Printf("[WARN] Prompt archive persistence failed request_id=%s: %v\n", record.RequestID, err) }; case <-store.stop: for { select { case record := <-store.jobs: _ = store.Insert(context.Background(), record); default: return } } } } }()
}

func (store *Store) Enqueue(record Record) bool {
	if store == nil { return false }
	store.lifecycleMu.Lock(); defer store.lifecycleMu.Unlock()
	if store.closing || store.jobs == nil { return false }
	select { case store.jobs <- record: return true; default: store.dropped.Add(1); return false }
}

func (store *Store) Insert(ctx context.Context, record Record) error {
	record.RequestID = strings.TrimSpace(record.RequestID); if record.RequestID == "" { return errors.New("prompt archive request ID is required") }
	if record.CreatedAt.IsZero() { record.CreatedAt = store.now().UTC() }
	payload := []byte(record.Payload)
	if len(payload) == 0 { payload = []byte(`{}`) }
	hashBytes := sha256.Sum256(payload); hash := hex.EncodeToString(hashBytes[:])
	store.dbMu.Lock(); defer store.dbMu.Unlock()
	tx, err := store.db.BeginTx(ctx, nil); if err != nil { return err }
	committed := false
	defer func(){ if !committed { _ = tx.Rollback() } }()
	_, err = tx.ExecContext(ctx, `INSERT INTO prompt_archive (request_id, created_at, model, message_count, tool_count, system_bytes, raw_bytes, summary, truncated, payload_hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(request_id) DO UPDATE SET created_at=excluded.created_at, model=excluded.model, message_count=excluded.message_count, tool_count=excluded.tool_count, system_bytes=excluded.system_bytes, raw_bytes=excluded.raw_bytes, summary=excluded.summary, truncated=excluded.truncated, payload_hash=excluded.payload_hash`, record.RequestID, toMillis(record.CreatedAt), record.Model, record.MessageCount, record.ToolCount, record.SystemBytes, record.RawBytes, record.Summary, boolInt(record.Truncated), hash)
	if err != nil { return err }
	_, err = tx.ExecContext(ctx, `INSERT INTO prompt_archive_payloads (request_id, created_at, payload) VALUES (?, ?, ?) ON CONFLICT(request_id) DO UPDATE SET created_at=excluded.created_at, payload=excluded.payload`, record.RequestID, toMillis(record.CreatedAt), payload)
	if err != nil { return err }
	if err := tx.Commit(); err != nil { return err }
	committed = true
	return nil
}

func (store *Store) Query(ctx context.Context, query Query) ([]Record, error) {
	where, args := buildWhere(query)
	statement := `SELECT request_id, created_at, model, message_count, tool_count, system_bytes, raw_bytes, summary, truncated FROM prompt_archive` + where + ` ORDER BY created_at DESC, request_id DESC`
	limit := query.Limit; if limit <= 0 { limit = 50 }; if limit > 200 { limit = 200 }
	statement += " LIMIT ?"; args = append(args, limit)
	if query.Offset > 0 { statement += " OFFSET ?"; args = append(args, query.Offset) }
	store.dbMu.Lock(); defer store.dbMu.Unlock()
	rows, err := store.db.QueryContext(ctx, statement, args...); if err != nil { return nil, err }
	defer rows.Close()
	var records []Record
	for rows.Next() { var r Record; var created int64; var truncated int; if err := rows.Scan(&r.RequestID, &created, &r.Model, &r.MessageCount, &r.ToolCount, &r.SystemBytes, &r.RawBytes, &r.Summary, &truncated); err != nil { return nil, err }; r.CreatedAt = fromMillis(created); r.Truncated = truncated != 0; records = append(records, r) }
	return records, rows.Err()
}

func (store *Store) Count(ctx context.Context, query Query) (int, error) {
	where, args := buildWhere(query); var total int
	store.dbMu.Lock(); defer store.dbMu.Unlock()
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM prompt_archive"+where, args...).Scan(&total); err != nil { return 0, err }
	return total, nil
}

func (store *Store) Detail(ctx context.Context, requestID string) (Record, error) {
	store.dbMu.Lock(); defer store.dbMu.Unlock()
	var r Record; var created int64; var truncated int; var payload []byte
	err := store.db.QueryRowContext(ctx, `SELECT a.request_id, a.created_at, a.model, a.message_count, a.tool_count, a.system_bytes, a.raw_bytes, a.summary, a.truncated, p.payload FROM prompt_archive a LEFT JOIN prompt_archive_payloads p ON p.request_id=a.request_id WHERE a.request_id=?`, requestID).Scan(&r.RequestID, &created, &r.Model, &r.MessageCount, &r.ToolCount, &r.SystemBytes, &r.RawBytes, &r.Summary, &truncated, &payload)
	if err != nil { return Record{}, err }
	r.CreatedAt = fromMillis(created); r.Truncated = truncated != 0; r.Payload = append(json.RawMessage(nil), payload...)
	return r, nil
}

func (store *Store) Delete(ctx context.Context, requestID string) (bool, error) {
	store.dbMu.Lock(); defer store.dbMu.Unlock()
	res, err := store.db.ExecContext(ctx, "DELETE FROM prompt_archive WHERE request_id = ?", requestID); if err != nil { return false, err }
	count, err := res.RowsAffected(); if err != nil { return false, err }
	return count > 0, nil
}

func BuildRecord(requestID string, body []byte, req models.ClaudeRequest, now time.Time) Record {
	payloadBody := body
	truncated := false
	if len(payloadBody) > DefaultMaxPayloadBytes { payloadBody = payloadBody[:DefaultMaxPayloadBytes]; truncated = true }
	redacted, err := diagnostics.RedactSecretsJSON(payloadBody)
	if err != nil { redacted = []byte(`{"error":"payload unavailable"}`); truncated = true }
	return Record{RequestID: requestID, CreatedAt: now.UTC(), Model: req.Model, MessageCount: len(req.Messages), ToolCount: len(req.Tools), SystemBytes: jsonSize(req.System), RawBytes: len(body), Summary: summarizeRequest(req), Truncated: truncated, Payload: redacted}
}

func summarizeRequest(req models.ClaudeRequest) string {
	parts := []string{fmt.Sprintf("%d 条消息", len(req.Messages))}
	if len(req.Tools) > 0 { parts = append(parts, fmt.Sprintf("%d 个工具", len(req.Tools))) }
	if req.MaxTokens > 0 { parts = append(parts, fmt.Sprintf("max_tokens=%d", req.MaxTokens)) }
	return strings.Join(parts, " · ")
}

func jsonSize(value any) int { if value == nil { return 0 }; b, _ := json.Marshal(value); return len(b) }

func buildWhere(query Query) (string, []any) {
	var clauses []string; var args []any
	if query.RequestID != "" { clauses = append(clauses, "request_id = ?"); args = append(args, query.RequestID) }
	if query.Model != "" { clauses = append(clauses, "model = ?"); args = append(args, query.Model) }
	if query.Keyword != "" { clauses = append(clauses, "(request_id LIKE ? OR model LIKE ? OR summary LIKE ?)"); like := "%"+query.Keyword+"%"; args = append(args, like, like, like) }
	if !query.Since.IsZero() { clauses = append(clauses, "created_at >= ?"); args = append(args, toMillis(query.Since)) }
	if !query.Until.IsZero() { clauses = append(clauses, "created_at <= ?"); args = append(args, toMillis(query.Until)) }
	if len(clauses) == 0 { return "", args }
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func boolInt(v bool) int { if v { return 1 }; return 0 }
func toMillis(v time.Time) int64 { return v.UTC().UnixMilli() }
func fromMillis(v int64) time.Time { return time.UnixMilli(v).UTC() }
