package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eruca/goagents/runkit"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const SchemaVersion = 2

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Create(ctx context.Context, record runkit.RunRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(record.RunID) == "" {
		return fmt.Errorf("run id is required")
	}
	now := time.Now()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now

	row, err := encodeRun(record)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO agent_runs (
	run_id, workflow_id, task_id, status, summary_status, content_ref,
	abort_reason, input_tokens, output_tokens, llm_calls, tool_calls,
	used_tools_json, metadata_json, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id) DO UPDATE SET
	workflow_id=excluded.workflow_id,
	task_id=excluded.task_id,
	status=excluded.status,
	summary_status=excluded.summary_status,
	content_ref=excluded.content_ref,
	abort_reason=excluded.abort_reason,
	input_tokens=excluded.input_tokens,
	output_tokens=excluded.output_tokens,
	llm_calls=excluded.llm_calls,
	tool_calls=excluded.tool_calls,
	used_tools_json=excluded.used_tools_json,
	metadata_json=excluded.metadata_json,
	created_at=excluded.created_at,
	updated_at=excluded.updated_at
`, row.RunID, row.WorkflowID, row.TaskID, row.Status, row.SummaryStatus, row.ContentRef,
		row.AbortReason, row.InputTokens, row.OutputTokens, row.LLMCalls, row.ToolCalls,
		row.UsedToolsJSON, row.MetadataJSON, row.CreatedAt, row.UpdatedAt)
	return err
}

func (s *Store) Get(ctx context.Context, runID string) (runkit.RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return runkit.RunRecord{}, err
	}
	row := runRow{}
	err := s.db.QueryRowContext(ctx, `
SELECT run_id, workflow_id, task_id, status, summary_status, content_ref,
	abort_reason, input_tokens, output_tokens, llm_calls, tool_calls,
	used_tools_json, metadata_json, created_at, updated_at
FROM agent_runs
WHERE run_id = ?
`, runID).Scan(&row.RunID, &row.WorkflowID, &row.TaskID, &row.Status, &row.SummaryStatus,
		&row.ContentRef, &row.AbortReason, &row.InputTokens, &row.OutputTokens,
		&row.LLMCalls, &row.ToolCalls, &row.UsedToolsJSON, &row.MetadataJSON,
		&row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return runkit.RunRecord{}, fmt.Errorf("%w: %s", runkit.ErrRunNotFound, runID)
	}
	if err != nil {
		return runkit.RunRecord{}, err
	}
	return decodeRun(row)
}

func (s *Store) AppendEvent(ctx context.Context, event runkit.RunEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(event.RunID) == "" {
		return fmt.Errorf("run id is required")
	}
	if event.RecordedAt.IsZero() {
		event.RecordedAt = time.Now()
	}
	metadata, err := marshalJSON(event.Metadata)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM agent_runs WHERE run_id = ?`, event.RunID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", runkit.ErrRunNotFound, event.RunID)
	}
	if err != nil {
		return err
	}

	var sequence int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM run_events WHERE run_id = ?`, event.RunID).Scan(&sequence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO run_events (
	run_id, sequence, event_type, stage, iteration, message, metadata_json, recorded_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, event.RunID, sequence, event.Type, event.Stage, event.Iteration, event.Message, metadata, event.RecordedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Events(ctx context.Context, runID string) ([]runkit.RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.requireRun(ctx, runID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, sequence, event_type, stage, iteration, message, metadata_json, recorded_at
FROM run_events
WHERE run_id = ?
ORDER BY sequence ASC
`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []runkit.RunEvent
	for rows.Next() {
		row := eventRow{}
		if err := rows.Scan(&row.RunID, &row.Sequence, &row.Type, &row.Stage, &row.Iteration, &row.Message, &row.MetadataJSON, &row.RecordedAt); err != nil {
			return nil, err
		}
		event, err := decodeEvent(row)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func (s *Store) Complete(ctx context.Context, runID string, summary runkit.TerminalSummary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	usedTools, err := marshalJSON(summary.UsedTools)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE agent_runs SET
	status = ?,
	summary_status = ?,
	content_ref = ?,
	abort_reason = ?,
	input_tokens = ?,
	output_tokens = ?,
	llm_calls = ?,
	tool_calls = ?,
	used_tools_json = ?,
	updated_at = ?
WHERE run_id = ?
`, summary.Status, summary.Status, summary.ContentRef, summary.AbortReason,
		summary.InputTokens, summary.OutputTokens, summary.LLMCalls, summary.ToolCalls,
		usedTools, time.Now(), runID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s", runkit.ErrRunNotFound, runID)
	}
	return nil
}

func (s *Store) FindByWorkflowID(ctx context.Context, workflowID string) ([]runkit.RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, workflow_id, task_id, status, summary_status, content_ref,
	abort_reason, input_tokens, output_tokens, llm_calls, tool_calls,
	used_tools_json, metadata_json, created_at, updated_at
FROM agent_runs
WHERE workflow_id = ?
ORDER BY created_at ASC, rowid ASC
`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []runkit.RunRecord
	for rows.Next() {
		row := runRow{}
		if err := rows.Scan(&row.RunID, &row.WorkflowID, &row.TaskID, &row.Status, &row.SummaryStatus,
			&row.ContentRef, &row.AbortReason, &row.InputTokens, &row.OutputTokens,
			&row.LLMCalls, &row.ToolCalls, &row.UsedToolsJSON, &row.MetadataJSON,
			&row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, err
		}
		record, err := decodeRun(row)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Store) requireRun(ctx context.Context, runID string) error {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM agent_runs WHERE run_id = ?`, runID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", runkit.ErrRunNotFound, runID)
	}
	return err
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS runkit_schema (
	id TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	updated_at TIMESTAMP NOT NULL
);

INSERT INTO runkit_schema (id, version, updated_at)
VALUES ('sqlitestore', ?, CURRENT_TIMESTAMP)
ON CONFLICT(id) DO UPDATE SET
	version=excluded.version,
	updated_at=excluded.updated_at;

CREATE TABLE IF NOT EXISTS agent_runs (
	run_id TEXT PRIMARY KEY,
	workflow_id TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	summary_status TEXT NOT NULL DEFAULT '',
	content_ref TEXT NOT NULL DEFAULT '',
	abort_reason TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	llm_calls INTEGER NOT NULL DEFAULT 0,
	tool_calls INTEGER NOT NULL DEFAULT 0,
	used_tools_json TEXT NOT NULL DEFAULT '[]',
	metadata_json TEXT NOT NULL DEFAULT '{}',
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_runs_workflow_id
ON agent_runs(workflow_id, created_at);

CREATE TABLE IF NOT EXISTS run_events (
	run_id TEXT NOT NULL,
	sequence INTEGER NOT NULL,
	event_type TEXT NOT NULL,
	stage TEXT NOT NULL DEFAULT '',
	iteration INTEGER NOT NULL DEFAULT 0,
	message TEXT NOT NULL DEFAULT '',
	metadata_json TEXT NOT NULL DEFAULT '{}',
	recorded_at TIMESTAMP NOT NULL,
	PRIMARY KEY (run_id, sequence),
	FOREIGN KEY (run_id) REFERENCES agent_runs(run_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_run_events_run_id_sequence
ON run_events(run_id, sequence);

CREATE TABLE IF NOT EXISTS approval_checkpoints (
 checkpoint_id TEXT PRIMARY KEY,
 run_id TEXT NOT NULL,
 tenant_id TEXT NOT NULL,
 definition_hash TEXT NOT NULL,
 ciphertext BLOB NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('pending', 'leased', 'consumed', 'rejected', 'failed', 'expired')),
 failure_code TEXT NOT NULL DEFAULT '',
 lease_owner TEXT NOT NULL DEFAULT '',
 lease_until TIMESTAMP,
 expires_at TIMESTAMP NOT NULL,
 created_at TIMESTAMP NOT NULL,
 updated_at TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_approval_checkpoints_tenant_status_expiry
ON approval_checkpoints(tenant_id, status, expires_at);

CREATE TABLE IF NOT EXISTS approval_decisions (
 checkpoint_id TEXT PRIMARY KEY REFERENCES approval_checkpoints(checkpoint_id) ON DELETE CASCADE,
 approver_id TEXT NOT NULL,
 approved INTEGER NOT NULL CHECK (approved IN (0, 1)),
 audit_ref TEXT NOT NULL,
 reason_code TEXT NOT NULL DEFAULT '',
 decided_at TIMESTAMP NOT NULL
);
`, SchemaVersion)
	return err
}

type runRow struct {
	RunID         string
	WorkflowID    string
	TaskID        string
	Status        string
	SummaryStatus string
	ContentRef    string
	AbortReason   string
	InputTokens   int
	OutputTokens  int
	LLMCalls      int
	ToolCalls     int
	UsedToolsJSON string
	MetadataJSON  string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type eventRow struct {
	RunID        string
	Sequence     int
	Type         string
	Stage        string
	Iteration    int
	Message      string
	MetadataJSON string
	RecordedAt   time.Time
}

func encodeRun(record runkit.RunRecord) (runRow, error) {
	usedTools, err := marshalJSON(record.Summary.UsedTools)
	if err != nil {
		return runRow{}, err
	}
	metadata, err := marshalJSON(record.Metadata)
	if err != nil {
		return runRow{}, err
	}
	return runRow{
		RunID:         record.RunID,
		WorkflowID:    record.WorkflowID,
		TaskID:        record.TaskID,
		Status:        string(record.Status),
		SummaryStatus: string(record.Summary.Status),
		ContentRef:    record.Summary.ContentRef,
		AbortReason:   record.Summary.AbortReason,
		InputTokens:   record.Summary.InputTokens,
		OutputTokens:  record.Summary.OutputTokens,
		LLMCalls:      record.Summary.LLMCalls,
		ToolCalls:     record.Summary.ToolCalls,
		UsedToolsJSON: usedTools,
		MetadataJSON:  metadata,
		CreatedAt:     record.CreatedAt,
		UpdatedAt:     record.UpdatedAt,
	}, nil
}

func decodeRun(row runRow) (runkit.RunRecord, error) {
	var usedTools []string
	if err := unmarshalJSON(row.UsedToolsJSON, &usedTools); err != nil {
		return runkit.RunRecord{}, err
	}
	var metadata map[string]any
	if err := unmarshalJSON(row.MetadataJSON, &metadata); err != nil {
		return runkit.RunRecord{}, err
	}
	return runkit.RunRecord{
		RunID:      row.RunID,
		WorkflowID: row.WorkflowID,
		TaskID:     row.TaskID,
		Status:     runkit.Status(row.Status),
		Summary: runkit.TerminalSummary{
			Status:       runkit.Status(row.SummaryStatus),
			ContentRef:   row.ContentRef,
			AbortReason:  row.AbortReason,
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
			LLMCalls:     row.LLMCalls,
			ToolCalls:    row.ToolCalls,
			UsedTools:    usedTools,
		},
		Metadata:  metadata,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

func decodeEvent(row eventRow) (runkit.RunEvent, error) {
	var metadata map[string]any
	if err := unmarshalJSON(row.MetadataJSON, &metadata); err != nil {
		return runkit.RunEvent{}, err
	}
	return runkit.RunEvent{
		RunID:      row.RunID,
		Sequence:   row.Sequence,
		Type:       row.Type,
		Stage:      row.Stage,
		Iteration:  row.Iteration,
		Message:    row.Message,
		Metadata:   metadata,
		RecordedAt: row.RecordedAt,
	}, nil
}

func marshalJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal runkit field: %w", err)
	}
	return string(data), nil
}

func unmarshalJSON[T any](data string, out *T) error {
	if data == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(data), out); err != nil {
		return fmt.Errorf("unmarshal runkit field: %w", err)
	}
	return nil
}
