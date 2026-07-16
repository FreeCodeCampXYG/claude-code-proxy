package diagnostics

const schemaVersion = 1

const createSchemaSQL = `
CREATE TABLE IF NOT EXISTS diagnostics_events (
	request_id TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	status_code INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	streaming INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	request_body BLOB,
	response_body BLOB,
	metadata BLOB
);
CREATE INDEX IF NOT EXISTS idx_diagnostics_events_created_at
	ON diagnostics_events(created_at DESC, request_id DESC);
CREATE INDEX IF NOT EXISTS idx_diagnostics_events_model
	ON diagnostics_events(model, created_at DESC);
PRAGMA user_version = 1;
`
