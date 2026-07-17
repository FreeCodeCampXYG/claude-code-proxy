package diagnostics

const schemaVersion = 5

const createSchemaV5SQL = `
CREATE TABLE diagnostics_events (
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
	metadata BLOB,
	attempt_count INTEGER NOT NULL DEFAULT 0,
	retry_count INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
	chunk_count INTEGER NOT NULL DEFAULT 0,
	stop_reason TEXT NOT NULL DEFAULT '',
	completion_state TEXT NOT NULL DEFAULT '',
	failure_kind TEXT NOT NULL DEFAULT '',
	canceled INTEGER NOT NULL DEFAULT 0,
	truncated INTEGER NOT NULL DEFAULT 0,
	api_key_label TEXT NOT NULL DEFAULT '',
	task_hash TEXT NOT NULL DEFAULT '',
	claude_request_bytes INTEGER NOT NULL DEFAULT 0,
	upstream_request_bytes INTEGER NOT NULL DEFAULT 0,
	upstream_response_bytes INTEGER NOT NULL DEFAULT 0,
	claude_response_bytes INTEGER NOT NULL DEFAULT 0,
	message_count INTEGER NOT NULL DEFAULT 0,
	tool_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE diagnostics_content (
	request_id TEXT NOT NULL,
	attempt_number INTEGER NOT NULL,
	boundary TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	capture_mode TEXT NOT NULL,
	content BLOB NOT NULL,
	PRIMARY KEY (request_id, attempt_number, boundary),
	FOREIGN KEY (request_id) REFERENCES diagnostics_events(request_id) ON DELETE CASCADE
);
CREATE INDEX idx_diagnostics_events_created_at
	ON diagnostics_events(created_at DESC, request_id DESC);
CREATE INDEX idx_diagnostics_events_model
	ON diagnostics_events(model, created_at DESC);
CREATE INDEX idx_diagnostics_events_task_hash
	ON diagnostics_events(task_hash, created_at DESC);
CREATE INDEX idx_diagnostics_content_created_at
	ON diagnostics_content(created_at DESC, request_id DESC, attempt_number DESC, boundary);
CREATE INDEX idx_diagnostics_content_expires_at
	ON diagnostics_content(expires_at, request_id);
PRAGMA user_version = 5;
`

var migrateV1ToV2Statements = []string{
	`ALTER TABLE diagnostics_events ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN cache_read_input_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN chunk_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN stop_reason TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE diagnostics_events ADD COLUMN completion_state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE diagnostics_events ADD COLUMN failure_kind TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE diagnostics_events ADD COLUMN canceled INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN truncated INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN task_hash TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_diagnostics_events_task_hash ON diagnostics_events(task_hash, created_at DESC)`,
	`PRAGMA user_version = 2`,
}

var migrateV2ToV3Statements = []string{
	`CREATE TABLE diagnostics_content (
		request_id TEXT NOT NULL,
		attempt_number INTEGER NOT NULL,
		boundary TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		capture_mode TEXT NOT NULL,
		content BLOB NOT NULL,
		PRIMARY KEY (request_id, attempt_number, boundary),
		FOREIGN KEY (request_id) REFERENCES diagnostics_events(request_id) ON DELETE CASCADE
	)`,
	`CREATE INDEX idx_diagnostics_content_created_at ON diagnostics_content(created_at DESC, request_id DESC, attempt_number DESC, boundary)`,
	`CREATE INDEX idx_diagnostics_content_expires_at ON diagnostics_content(expires_at, request_id)`,
	`PRAGMA user_version = 3`,
}

var migrateV3ToV4Statements = []string{
	`ALTER TABLE diagnostics_events ADD COLUMN api_key_label TEXT NOT NULL DEFAULT ''`,
	`PRAGMA user_version = 4`,
}

var migrateV4ToV5Statements = []string{
	`ALTER TABLE diagnostics_events ADD COLUMN claude_request_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN upstream_request_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN upstream_response_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN claude_response_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN message_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE diagnostics_events ADD COLUMN tool_count INTEGER NOT NULL DEFAULT 0`,
	`PRAGMA user_version = 5`,
}
