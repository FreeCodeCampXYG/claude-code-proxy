package promptarchive

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
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
	DefaultRetention       = 168 * time.Hour
	DefaultMaxPayloadBytes = 5 * 1024 * 1024
	maxDeltaDepth          = 20

	archiveSelectColumns = `request_id,created_at,model,message_count,tool_count,system_bytes,raw_bytes,summary,truncated,storage_kind,stored_bytes,reconstructed_bytes,payload_state`
	archiveSchema = `
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
			payload_hash TEXT NOT NULL DEFAULT '',
			session_key TEXT NOT NULL DEFAULT '',
			storage_kind TEXT NOT NULL DEFAULT 'full',
			base_request_id TEXT NOT NULL DEFAULT '',
			delta_depth INTEGER NOT NULL DEFAULT 0,
			stored_bytes INTEGER NOT NULL DEFAULT 0,
			reconstructed_bytes INTEGER NOT NULL DEFAULT 0,
			payload_state TEXT NOT NULL DEFAULT 'available'
		);
		CREATE TABLE IF NOT EXISTS prompt_archive_payloads (
			request_id TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			payload BLOB NOT NULL,
			FOREIGN KEY (request_id) REFERENCES prompt_archive(request_id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS prompt_archive_metadata (
			name TEXT PRIMARY KEY,
			value BLOB NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_prompt_archive_created_at ON prompt_archive(created_at DESC,request_id DESC);`
)

var archiveMigrationColumns = []string{
	"session_key TEXT NOT NULL DEFAULT ''",
	"storage_kind TEXT NOT NULL DEFAULT 'full'",
	"base_request_id TEXT NOT NULL DEFAULT ''",
	"delta_depth INTEGER NOT NULL DEFAULT 0",
	"stored_bytes INTEGER NOT NULL DEFAULT 0",
	"reconstructed_bytes INTEGER NOT NULL DEFAULT 0",
	"payload_state TEXT NOT NULL DEFAULT 'available'",
}

type Options struct {
	Retention   time.Duration
	BusyTimeout time.Duration
	Now         func() time.Time
}

type Store struct {
	db             *sql.DB
	retention      time.Duration
	now            func() time.Time
	jobs           chan Record
	stop           chan struct{}
	worker         sync.WaitGroup
	dropped        atomic.Uint64
	dbMu           sync.Mutex
	closing        bool
	lifecycleMu    sync.Mutex
	sessionHMACKey []byte
}

type Record struct {
	RequestID          string          `json:"request_id"`
	CreatedAt          time.Time       `json:"created_at"`
	Model              string          `json:"model"`
	MessageCount       int             `json:"message_count"`
	ToolCount          int             `json:"tool_count"`
	SystemBytes        int             `json:"system_bytes"`
	RawBytes           int             `json:"raw_bytes"`
	Summary            string          `json:"summary"`
	Truncated          bool            `json:"truncated"`
	Payload            json.RawMessage `json:"payload,omitempty"`
	StorageKind        string          `json:"storage_kind,omitempty"`
	Incremental        bool            `json:"incremental,omitempty"`
	StoredBytes        int             `json:"stored_bytes,omitempty"`
	ReconstructedBytes int             `json:"reconstructed_bytes,omitempty"`
	PayloadState       string          `json:"payload_state,omitempty"`
	sessionID          string
}

type Query struct {
	RequestID string
	Model     string
	Keyword   string
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
}

type Statistics struct {
	FullCount          int   `json:"full_count"`
	DeltaCount         int   `json:"delta_count"`
	StoredBytes        int64 `json:"stored_bytes"`
	ReconstructedBytes int64 `json:"reconstructed_bytes"`
}

func Open(path string, options Options) (*Store, error) {
	path, wal, err := prepareDatabasePath(path)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open prompt archive database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := newStore(db, options)
	ctx, cancel := context.WithTimeout(context.Background(), storeBusyTimeout(options)+time.Second)
	defer cancel()
	if err := store.initialize(ctx, storeBusyTimeout(options), wal); err != nil {
		_ = db.Close()
		return nil, err
	}
	store.startWriter()
	return store, nil
}

func prepareDatabasePath(path string) (string, bool, error) {
	if strings.TrimSpace(path) == "" {
		return "", false, errors.New("prompt archive database path is required")
	}
	if path == ":memory:" {
		return path, false, nil
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", false, fmt.Errorf("resolve prompt archive database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return "", false, fmt.Errorf("create prompt archive database directory: %w", err)
	}
	return absolute, true, nil
}

func newStore(db *sql.DB, options Options) *Store {
	retention := options.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, retention: retention, now: now}
}

func storeBusyTimeout(options Options) time.Duration {
	if options.BusyTimeout > 0 {
		return options.BusyTimeout
	}
	return 5 * time.Second
}

func (s *Store) initialize(ctx context.Context, busy time.Duration, wal bool) error {
	if err := s.configureDatabase(ctx, busy, wal); err != nil {
		return err
	}
	if err := s.createSchema(ctx); err != nil {
		return err
	}
	if err := s.migrateSchema(ctx); err != nil {
		return err
	}
	if err := s.loadSessionHMACKey(ctx); err != nil {
		return err
	}
	return s.deleteExpired(ctx)
}

func (s *Store) configureDatabase(ctx context.Context, busy time.Duration, wal bool) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping prompt archive database: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busy.Milliseconds())); err != nil {
		return err
	}
	if wal {
		if _, err := s.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	return err
}

func (s *Store) createSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, archiveSchema); err != nil {
		return fmt.Errorf("create prompt archive schema: %w", err)
	}
	return nil
}

func (s *Store) migrateSchema(ctx context.Context) error {
	for _, column := range archiveMigrationColumns {
		statement := "ALTER TABLE prompt_archive ADD COLUMN " + column
		if _, err := s.db.ExecContext(ctx, statement); err != nil && !isDuplicateColumnError(err) {
			return fmt.Errorf("migrate prompt archive column %q: %w", column, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_prompt_archive_session ON prompt_archive(session_key,created_at DESC,request_id DESC)"); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE prompt_archive
		SET stored_bytes=COALESCE((SELECT length(payload) FROM prompt_archive_payloads p WHERE p.request_id=prompt_archive.request_id),0),
			reconstructed_bytes=COALESCE((SELECT length(payload) FROM prompt_archive_payloads p WHERE p.request_id=prompt_archive.request_id),0)
		WHERE stored_bytes=0`)
	return err
}

func isDuplicateColumnError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

func (s *Store) loadSessionHMACKey(ctx context.Context) error {
	var key []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM prompt_archive_metadata WHERE name='session_hmac_key'").Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "INSERT INTO prompt_archive_metadata(name,value) VALUES('session_hmac_key',?)", key); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	s.sessionHMACKey = key
	return nil
}

func (s *Store) deleteExpired(ctx context.Context) error {
	cutoff := toMillis(s.now().UTC().Add(-s.retention))
	rows, err := s.db.QueryContext(ctx, "SELECT request_id FROM prompt_archive WHERE created_at < ?", cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()

	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		expired = append(expired, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range expired {
		if _, err := s.deleteLocked(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.lifecycleMu.Lock()
	if s.closing {
		s.lifecycleMu.Unlock()
		return nil
	}
	s.closing = true
	close(s.stop)
	s.lifecycleMu.Unlock()
	s.worker.Wait()
	return s.db.Close()
}

func (s *Store) startWriter() {
	s.jobs = make(chan Record, 256)
	s.stop = make(chan struct{})
	s.worker.Add(1)
	go func() {
		defer s.worker.Done()
		for {
			select {
			case record := <-s.jobs:
				s.persistQueuedRecord(record)
			case <-s.stop:
				s.drainQueuedRecords()
				return
			}
		}
	}()
}

func (s *Store) persistQueuedRecord(record Record) {
	if err := s.Insert(context.Background(), record); err != nil {
		s.dropped.Add(1)
		fmt.Printf("[WARN] Prompt archive persistence failed request_id=%s: %v\n", record.RequestID, err)
	}
}

func (s *Store) drainQueuedRecords() {
	deadline, cancel := context.WithTimeout(context.Background(), storeBusyTimeout(Options{}))
	defer cancel()
	for {
		select {
		case record := <-s.jobs:
			if err := s.Insert(deadline, record); err != nil {
				s.dropped.Add(1)
			}
		default:
			return
		}
	}
}

func (s *Store) Enqueue(record Record) bool {
	if s == nil {
		return false
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing || s.jobs == nil {
		return false
	}
	select {
	case s.jobs <- record:
		return true
	default:
		s.dropped.Add(1)
		return false
	}
}

func (s *Store) sessionKey(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(s.sessionHMACKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, s.sessionHMACKey)
	_, _ = mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil))
}

type insertRecord struct {
	Record
	payload []byte
	hash    string
	key     string
}

type storageDecision struct {
	kind    string
	base    string
	depth   int
	payload []byte
}

func (s *Store) Insert(ctx context.Context, record Record) error {
	prepared, err := s.prepareInsert(record)
	if err != nil {
		return err
	}

	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	return s.insertPrepared(ctx, prepared)
}

func (s *Store) prepareInsert(record Record) (insertRecord, error) {
	record.RequestID = strings.TrimSpace(record.RequestID)
	if record.RequestID == "" {
		return insertRecord{}, errors.New("prompt archive request ID is required")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = s.now().UTC()
	}
	payload := []byte(record.Payload)
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	return insertRecord{
		Record:  record,
		payload: payload,
		hash:    hashPayload(payload),
		key:     s.sessionKey(record.sessionID),
	}, nil
}

func (s *Store) insertPrepared(ctx context.Context, prepared insertRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := s.materializeDependentsTx(ctx, tx, prepared.RequestID); err != nil {
		return err
	}
	decision := s.chooseStorageTx(ctx, tx, prepared)
	if err := persistRecordTx(ctx, tx, prepared, decision); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Store) chooseStorageTx(ctx context.Context, tx *sql.Tx, prepared insertRecord) storageDecision {
	decision := storageDecision{kind: "full", payload: prepared.payload}
	if prepared.key == "" || prepared.Truncated {
		return decision
	}

	var id, previousHash string
	var previousDepth int
	err := tx.QueryRowContext(ctx, `SELECT request_id,payload_hash,delta_depth FROM prompt_archive WHERE session_key=? AND request_id<>? AND payload_state='available' ORDER BY created_at DESC,request_id DESC LIMIT 1`, prepared.key, prepared.RequestID).Scan(&id, &previousHash, &previousDepth)
	if err != nil || previousDepth >= maxDeltaDepth {
		return decision
	}
	basePayload, err := s.reconstructTx(ctx, tx, id)
	if err != nil || hashPayload(basePayload) != previousHash {
		return decision
	}
	delta, err := makeByteDelta(basePayload, prepared.payload)
	if err != nil || len(delta) >= len(prepared.payload)*9/10 {
		return decision
	}
	return storageDecision{kind: "delta", base: id, depth: previousDepth + 1, payload: delta}
}

func persistRecordTx(ctx context.Context, tx *sql.Tx, prepared insertRecord, decision storageDecision) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO prompt_archive(request_id,created_at,model,message_count,tool_count,system_bytes,raw_bytes,summary,truncated,payload_hash,session_key,storage_kind,base_request_id,delta_depth,stored_bytes,reconstructed_bytes,payload_state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET created_at=excluded.created_at,model=excluded.model,message_count=excluded.message_count,tool_count=excluded.tool_count,system_bytes=excluded.system_bytes,raw_bytes=excluded.raw_bytes,summary=excluded.summary,truncated=excluded.truncated,payload_hash=excluded.payload_hash,session_key=excluded.session_key,storage_kind=excluded.storage_kind,base_request_id=excluded.base_request_id,delta_depth=excluded.delta_depth,stored_bytes=excluded.stored_bytes,reconstructed_bytes=excluded.reconstructed_bytes,payload_state=excluded.payload_state`, prepared.RequestID, toMillis(prepared.CreatedAt), prepared.Model, prepared.MessageCount, prepared.ToolCount, prepared.SystemBytes, prepared.RawBytes, prepared.Summary, boolInt(prepared.Truncated), prepared.hash, prepared.key, decision.kind, decision.base, decision.depth, len(decision.payload), len(prepared.payload), "available"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO prompt_archive_payloads(request_id,created_at,payload) VALUES(?,?,?) ON CONFLICT(request_id) DO UPDATE SET created_at=excluded.created_at,payload=excluded.payload`, prepared.RequestID, toMillis(prepared.CreatedAt), decision.payload)
	return err
}

func hashPayload(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

type byteDelta struct {
	Prefix  int    `json:"prefix"`
	Suffix  int    `json:"suffix"`
	Replace []byte `json:"replace"`
}

func makeByteDelta(base, next []byte) ([]byte, error) {
	prefix := 0
	for prefix < len(base) && prefix < len(next) && base[prefix] == next[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(base)-prefix && suffix < len(next)-prefix && base[len(base)-1-suffix] == next[len(next)-1-suffix] {
		suffix++
	}
	return json.Marshal(byteDelta{Prefix: prefix, Suffix: suffix, Replace: next[prefix : len(next)-suffix]})
}

func applyByteDelta(base, encoded []byte) ([]byte, error) {
	var delta byteDelta
	if err := json.Unmarshal(encoded, &delta); err != nil {
		return nil, err
	}
	if delta.Prefix < 0 || delta.Suffix < 0 || delta.Prefix+delta.Suffix > len(base) {
		return nil, errors.New("invalid prompt archive delta")
	}
	payload := make([]byte, 0, delta.Prefix+len(delta.Replace)+delta.Suffix)
	payload = append(payload, base[:delta.Prefix]...)
	payload = append(payload, delta.Replace...)
	payload = append(payload, base[len(base)-delta.Suffix:]...)
	return payload, nil
}

type reconstructionContext struct {
	visited map[string]bool
	cache   map[string][]byte
}

func newReconstructionContext() *reconstructionContext {
	return &reconstructionContext{visited: make(map[string]bool), cache: make(map[string][]byte)}
}

func (s *Store) reconstructTx(ctx context.Context, tx *sql.Tx, id string) ([]byte, error) {
	return s.reconstructTxCached(ctx, tx, id, newReconstructionContext())
}

func (s *Store) reconstructTxCached(ctx context.Context, tx *sql.Tx, id string, rc *reconstructionContext) ([]byte, error) {
	if payload, ok := rc.cache[id]; ok {
		return append([]byte(nil), payload...), nil
	}
	if rc.visited[id] {
		return nil, errors.New("prompt archive delta cycle detected")
	}
	rc.visited[id] = true
	defer delete(rc.visited, id)

	var kind, base, hash string
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT a.storage_kind,a.base_request_id,a.payload_hash,p.payload FROM prompt_archive a JOIN prompt_archive_payloads p ON p.request_id=a.request_id WHERE a.request_id=?`, id).Scan(&kind, &base, &hash, &payload)
	if err != nil {
		return nil, err
	}
	if kind == "delta" {
		parent, err := s.reconstructTxCached(ctx, tx, base, rc)
		if err != nil {
			return nil, err
		}
		payload, err = applyByteDelta(parent, payload)
		if err != nil {
			return nil, err
		}
	}
	if hash != "" && hashPayload(payload) != hash {
		return nil, errors.New("prompt archive payload integrity check failed")
	}
	rc.cache[id] = append([]byte(nil), payload...)
	return append([]byte(nil), payload...), nil
}

func (s *Store) Query(ctx context.Context, query Query) ([]Record, error) {
	statement, args := buildQueryStatement(query)
	s.dbMu.Lock()
	defer s.dbMu.Unlock()

	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

func buildQueryStatement(query Query) (string, []any) {
	where, args := buildWhere(query)
	limit := normalizedQueryLimit(query.Limit)
	statement := `SELECT ` + archiveSelectColumns + ` FROM prompt_archive` + where + ` ORDER BY created_at DESC,request_id DESC LIMIT ?`
	args = append(args, limit)
	if query.Offset > 0 {
		statement += " OFFSET ?"
		args = append(args, query.Offset)
	}
	return statement, args
}

func normalizedQueryLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func (s *Store) Count(ctx context.Context, query Query) (int, error) {
	where, args := buildWhere(query)
	var count int
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM prompt_archive"+where, args...).Scan(&count)
	return count, err
}

func (s *Store) Detail(ctx context.Context, id string) (Record, error) {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	record, err := s.detailTx(ctx, tx, id, newReconstructionContext())
	if err != nil {
		return Record{}, err
	}
	return record, tx.Commit()
}

// Export returns complete records in one read transaction. Reconstruction shares
// a cache across records, avoiding repeated base-chain queries for related deltas.
func (s *Store) Export(ctx context.Context, query Query, max int) ([]Record, error) {
	max = normalizedExportLimit(max)
	query.Offset = 0
	query.Limit = max
	where, args := buildWhere(query)
	statement := `SELECT ` + archiveSelectColumns + ` FROM prompt_archive` + where + ` ORDER BY created_at DESC,request_id DESC LIMIT ?`
	args = append(args, max)

	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	return s.exportRecords(ctx, tx, rows, max)
}

func normalizedExportLimit(max int) int {
	if max <= 0 || max > 1000 {
		return 1000
	}
	return max
}

func (s *Store) exportRecords(ctx context.Context, tx *sql.Tx, rows *sql.Rows, max int) ([]Record, error) {
	// Read and close the cursor before reconstruction. SQLite uses a single
	// connection, so nested queries while rows is active can block the export.
	records := make([]Record, 0, max)
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rc := newReconstructionContext()
	for index := range records {
		payload, err := s.reconstructTxCached(ctx, tx, records[index].RequestID, rc)
		if err != nil {
			records[index].PayloadState = "unavailable"
			continue
		}
		records[index].Payload = payload
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return records, nil
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func scanRecord(scanner interface{ Scan(...any) error }) (Record, error) {
	var record Record
	var milliseconds int64
	var truncated int
	if err := scanner.Scan(&record.RequestID, &milliseconds, &record.Model, &record.MessageCount, &record.ToolCount, &record.SystemBytes, &record.RawBytes, &record.Summary, &truncated, &record.StorageKind, &record.StoredBytes, &record.ReconstructedBytes, &record.PayloadState); err != nil {
		return Record{}, err
	}
	record.CreatedAt = fromMillis(milliseconds)
	record.Truncated = truncated != 0
	record.Incremental = record.StorageKind == "delta"
	return record, nil
}

func (s *Store) detailTx(ctx context.Context, tx *sql.Tx, id string, rc *reconstructionContext) (Record, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+archiveSelectColumns+` FROM prompt_archive WHERE request_id=?`, id)
	record, err := scanRecord(row)
	if err != nil {
		return Record{}, err
	}
	payload, err := s.reconstructTxCached(ctx, tx, id, rc)
	if err != nil {
		record.PayloadState = "unavailable"
		return record, nil
	}
	record.Payload = payload
	return record, nil
}

func (s *Store) materializeDependentsTx(ctx context.Context, tx *sql.Tx, id string) error {
	children, err := dependentRecordIDs(ctx, tx, id)
	if err != nil {
		return err
	}
	for _, child := range children {
		if err := s.materializeDependentsTx(ctx, tx, child); err != nil {
			return err
		}
		payload, err := s.reconstructTx(ctx, tx, child)
		if err != nil {
			return err
		}
		if err := persistMaterializedPayload(ctx, tx, child, payload); err != nil {
			return err
		}
	}
	return nil
}

func dependentRecordIDs(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT request_id FROM prompt_archive WHERE base_request_id=?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var children []string
	for rows.Next() {
		var child string
		if err := rows.Scan(&child); err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return children, nil
}

func persistMaterializedPayload(ctx context.Context, tx *sql.Tx, id string, payload []byte) error {
	if _, err := tx.ExecContext(ctx, "UPDATE prompt_archive SET storage_kind='full',base_request_id='',delta_depth=0,stored_bytes=? WHERE request_id=?", len(payload), id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE prompt_archive_payloads SET payload=? WHERE request_id=?", payload, id)
	return err
}

func (s *Store) Delete(ctx context.Context, id string) (bool, error) {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	return s.deleteLocked(ctx, id)
}

func (s *Store) deleteLocked(ctx context.Context, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := s.materializeDependentsTx(ctx, tx, id); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM prompt_archive WHERE request_id=?", id)
	if err != nil {
		return false, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	return deleted > 0, nil
}

func (s *Store) Statistics(ctx context.Context) (Statistics, error) {
	var statistics Statistics
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN storage_kind='full' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN storage_kind='delta' THEN 1 ELSE 0 END),0),COALESCE(SUM(stored_bytes),0),COALESCE(SUM(reconstructed_bytes),0) FROM prompt_archive`).Scan(&statistics.FullCount, &statistics.DeltaCount, &statistics.StoredBytes, &statistics.ReconstructedBytes)
	return statistics, err
}

func BuildRecord(requestID string, body []byte, request models.ClaudeRequest, now time.Time, sessionID ...string) Record {
	truncated := len(body) > DefaultMaxPayloadBytes
	payload := body
	if truncated {
		payload = []byte(`{"archive":"payload unavailable: size limit exceeded"}`)
	} else if redacted, err := diagnostics.RedactSecretsJSON(payload); err == nil {
		payload = redacted
	} else {
		payload = []byte(`{"archive":"payload unavailable: invalid JSON"}`)
		truncated = true
	}

	record := Record{
		RequestID:    requestID,
		CreatedAt:    now.UTC(),
		Model:        request.Model,
		MessageCount: len(request.Messages),
		ToolCount:    len(request.Tools),
		SystemBytes:  jsonSize(request.System),
		RawBytes:     len(body),
		Summary:      summarizeRequest(request),
		Truncated:    truncated,
		Payload:      payload,
	}
	if len(sessionID) > 0 {
		record.sessionID = sessionID[0]
	}
	return record
}

func summarizeRequest(request models.ClaudeRequest) string {
	parts := []string{fmt.Sprintf("%d 条消息", len(request.Messages))}
	if len(request.Tools) > 0 {
		parts = append(parts, fmt.Sprintf("%d 个工具", len(request.Tools)))
	}
	if request.MaxTokens > 0 {
		parts = append(parts, fmt.Sprintf("max_tokens=%d", request.MaxTokens))
	}
	return strings.Join(parts, " · ")
}

func jsonSize(value any) int {
	if value == nil {
		return 0
	}
	encoded, _ := json.Marshal(value)
	return len(encoded)
}

func buildWhere(query Query) (string, []any) {
	var conditions []string
	var arguments []any
	if query.RequestID != "" {
		conditions = append(conditions, "request_id=?")
		arguments = append(arguments, query.RequestID)
	}
	if query.Model != "" {
		conditions = append(conditions, "model=?")
		arguments = append(arguments, query.Model)
	}
	if query.Keyword != "" {
		conditions = append(conditions, "(request_id LIKE ? OR model LIKE ? OR summary LIKE ?)")
		keyword := "%" + query.Keyword + "%"
		arguments = append(arguments, keyword, keyword, keyword)
	}
	if !query.Since.IsZero() {
		conditions = append(conditions, "created_at>=?")
		arguments = append(arguments, toMillis(query.Since))
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, "created_at<=?")
		arguments = append(arguments, toMillis(query.Until))
	}
	if len(conditions) == 0 {
		return "", arguments
	}
	return " WHERE " + strings.Join(conditions, " AND "), arguments
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func toMillis(value time.Time) int64 {
	return value.UTC().UnixMilli()
}

func fromMillis(value int64) time.Time {
	return time.UnixMilli(value).UTC()
}
