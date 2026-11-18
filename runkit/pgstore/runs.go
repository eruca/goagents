package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eruca/goagents/runkit"
)

var _ runkit.Store = (*Store)(nil)

// runColumns lists agent_runs columns in the order scanRun reads them.
const runColumns = `run_id, workflow_id, task_id, status, summary_status, content_ref,
	abort_reason, input_tokens, output_tokens, llm_calls, tool_calls,
	used_tools_json, metadata_json, created_at, updated_at`

// Create inserts a run, or replaces every field of an existing run with the
// same id, like sqlitestore. A replaced run keeps its insertion position for
// FindByWorkflowID ties.
func (s *Store) Create(ctx context.Context, record runkit.RunRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(record.RunID) == "" {
		return fmt.Errorf("run id is required")
	}
	now := pgTime(time.Now())
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	usedTools, err := marshalJSON(record.Summary.UsedTools)
	if err != nil {
		return err
	}
	metadata, err := marshalJSON(record.Metadata)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO agent_runs (`+runColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT (run_id) DO UPDATE SET
	workflow_id = excluded.workflow_id,
	task_id = excluded.task_id,
	status = excluded.status,
	summary_status = excluded.summary_status,
	content_ref = excluded.content_ref,
	abort_reason = excluded.abort_reason,
	input_tokens = excluded.input_tokens,
	output_tokens = excluded.output_tokens,
	llm_calls = excluded.llm_calls,
	tool_calls = excluded.tool_calls,
	used_tools_json = excluded.used_tools_json,
	metadata_json = excluded.metadata_json,
	created_at = excluded.created_at,
	updated_at = excluded.updated_at`,
		record.RunID, record.WorkflowID, record.TaskID, string(record.Status),
		string(record.Summary.Status), record.Summary.ContentRef, record.Summary.AbortReason,
		record.Summary.InputTokens, record.Summary.OutputTokens, record.Summary.LLMCalls,
		record.Summary.ToolCalls, usedTools, metadata, pgTime(record.CreatedAt), now)
	return err
}

func (s *Store) Get(ctx context.Context, runID string) (runkit.RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return runkit.RunRecord{}, err
	}
	record, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM agent_runs WHERE run_id = $1`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return runkit.RunRecord{}, fmt.Errorf("%w: %s", runkit.ErrRunNotFound, runID)
	}
	return record, err
}

// AppendEvent assigns the next per-run sequence. Locking the run row
// serializes appenders for one run across every connection pool, so the
// sequence is gap-free and never reused; appends to different runs do not
// wait on each other.
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
	defer func() { _ = tx.Rollback() }()

	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM agent_runs WHERE run_id = $1 FOR UPDATE`, event.RunID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", runkit.ErrRunNotFound, event.RunID)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO run_events (run_id, sequence, event_type, stage, iteration, message, metadata_json, recorded_at)
SELECT $1, COALESCE(MAX(sequence), 0) + 1, $2, $3, $4, $5, $6, $7
FROM run_events WHERE run_id = $1`,
		event.RunID, event.Type, event.Stage, event.Iteration, event.Message, metadata, pgTime(event.RecordedAt))
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
WHERE run_id = $1
ORDER BY sequence ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []runkit.RunEvent
	for rows.Next() {
		var event runkit.RunEvent
		var metadata string
		if err := rows.Scan(&event.RunID, &event.Sequence, &event.Type, &event.Stage, &event.Iteration, &event.Message, &metadata, &event.RecordedAt); err != nil {
			return nil, err
		}
		if err := unmarshalJSON(metadata, &event.Metadata); err != nil {
			return nil, err
		}
		event.RecordedAt = event.RecordedAt.UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// Complete overwrites the status and terminal summary; completing a run again
// replaces the earlier summary, like sqlitestore.
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
	status = $1,
	summary_status = $1,
	content_ref = $2,
	abort_reason = $3,
	input_tokens = $4,
	output_tokens = $5,
	llm_calls = $6,
	tool_calls = $7,
	used_tools_json = $8,
	updated_at = $9
WHERE run_id = $10`,
		string(summary.Status), summary.ContentRef, summary.AbortReason,
		summary.InputTokens, summary.OutputTokens, summary.LLMCalls, summary.ToolCalls,
		usedTools, pgTime(time.Now()), runID)
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

// FindByWorkflowID returns the workflow's runs by creation time, ties broken
// by first insertion, matching sqlitestore's created_at, rowid order.
func (s *Store) FindByWorkflowID(ctx context.Context, workflowID string) ([]runkit.RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+runColumns+`
FROM agent_runs
WHERE workflow_id = $1
ORDER BY created_at ASC, insert_order ASC`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []runkit.RunRecord
	for rows.Next() {
		record, err := scanRun(rows)
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
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM agent_runs WHERE run_id = $1`, runID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", runkit.ErrRunNotFound, runID)
	}
	return err
}

func scanRun(row rowScanner) (runkit.RunRecord, error) {
	var record runkit.RunRecord
	var status, summaryStatus, usedTools, metadata string
	err := row.Scan(&record.RunID, &record.WorkflowID, &record.TaskID, &status, &summaryStatus,
		&record.Summary.ContentRef, &record.Summary.AbortReason, &record.Summary.InputTokens,
		&record.Summary.OutputTokens, &record.Summary.LLMCalls, &record.Summary.ToolCalls,
		&usedTools, &metadata, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return runkit.RunRecord{}, err
	}
	record.Status = runkit.Status(status)
	record.Summary.Status = runkit.Status(summaryStatus)
	if err := unmarshalJSON(usedTools, &record.Summary.UsedTools); err != nil {
		return runkit.RunRecord{}, err
	}
	if err := unmarshalJSON(metadata, &record.Metadata); err != nil {
		return runkit.RunRecord{}, err
	}
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}

// pgTime matches what TIMESTAMPTZ stores, so a written time reads back equal.
func pgTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
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
