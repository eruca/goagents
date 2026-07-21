package pgstore

import (
	"context"
	"fmt"

	"github.com/eruca/goagents/memorykit"
	pgvector "github.com/pgvector/pgvector-go"
)

var _ memorykit.EmbeddingStore = (*Store)(nil)

func (s *Store) PendingEmbeddings(ctx context.Context, query memorykit.PendingEmbeddingQuery) ([]memorykit.EmbeddingInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := memorykit.ValidatePendingEmbeddingQuery(query, s.cfg.Limits); err != nil {
		return nil, err
	}
	if query.ProfileID != s.cfg.EmbeddingProfileID {
		return nil, fmt.Errorf("%w: embedding profile does not match store configuration", memorykit.ErrInvalidMemory)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.tenant_id, m.subject_type, m.subject_id,
		       m.content, m.content_hash, m.version
		FROM memories m
		LEFT JOIN memory_embeddings e
		  ON e.memory_id = m.id AND e.embedding_profile_id = $1
		WHERE m.status = 'active'
		  AND (e.memory_id IS NULL OR e.content_hash <> m.content_hash OR e.dimension <> $2)
		ORDER BY m.updated_at ASC, m.id ASC
		LIMIT $3`, query.ProfileID, s.cfg.EmbeddingDimensions, query.Limit)
	if err != nil {
		return nil, wrapBackend("pending embeddings", err)
	}
	defer rows.Close()

	result := make([]memorykit.EmbeddingInput, 0)
	for rows.Next() {
		var input memorykit.EmbeddingInput
		if err := rows.Scan(
			&input.MemoryID, &input.Scope.TenantID, &input.Scope.SubjectType, &input.Scope.SubjectID,
			&input.Content, &input.ContentHash, &input.Version,
		); err != nil {
			return nil, wrapBackend("scan pending embeddings", err)
		}
		result = append(result, input)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapBackend("read pending embeddings", err)
	}
	return append([]memorykit.EmbeddingInput(nil), result...), nil
}

func (s *Store) PutEmbedding(ctx context.Context, request memorykit.PutEmbeddingRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := memorykit.ValidatePutEmbeddingRequest(request, s.cfg.Limits); err != nil {
		return err
	}
	if request.ProfileID != s.cfg.EmbeddingProfileID || request.Dimensions != s.cfg.EmbeddingDimensions {
		return fmt.Errorf("%w: embedding request does not match store configuration", memorykit.ErrInvalidMemory)
	}

	// Lock the current memory row inside the conditional write. A correction
	// cannot change content_hash between validation and the embedding upsert.
	result, err := s.db.ExecContext(ctx, `
		WITH current_memory AS (
			SELECT id FROM memories
			WHERE id = $1 AND tenant_id = $2 AND subject_type = $3 AND subject_id = $4
			  AND status = 'active' AND content_hash = $5
			FOR UPDATE
		)
		INSERT INTO memory_embeddings
			(memory_id, embedding_profile_id, content_hash, dimension, embedding, embedded_at)
		SELECT id, $6, $5, $7, $8, $9 FROM current_memory
		ON CONFLICT (memory_id, embedding_profile_id) DO UPDATE SET
			content_hash = EXCLUDED.content_hash,
			dimension = EXCLUDED.dimension,
			embedding = EXCLUDED.embedding,
			embedded_at = EXCLUDED.embedded_at`,
		request.MemoryID, request.Scope.TenantID, request.Scope.SubjectType, request.Scope.SubjectID,
		request.ContentHash, request.ProfileID, request.Dimensions,
		pgvector.NewVector(append([]float32(nil), request.Vector...)), request.EmbeddedAt)
	if err != nil {
		return wrapBackend("put embedding", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return wrapBackend("put embedding result", err)
	}
	if affected != 1 {
		return memorykit.ErrConflict
	}
	return nil
}
