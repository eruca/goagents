package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/eruca/goagents/memorykit"
	pgvector "github.com/pgvector/pgvector-go"
)

var _ memorykit.RecallStore = (*Store)(nil)

const qualifiedMemoryColumns = `
	m.id, m.tenant_id, m.subject_type, m.subject_id, m.kind, m.memory_key,
	m.status, m.content, m.valid_from, m.valid_until, m.importance, m.confidence,
	m.source_agent_id, m.created_by, m.idempotency_key, m.version, m.created_at, m.updated_at`

func (s *Store) SearchCandidates(ctx context.Context, query memorykit.CandidateQuery) (memorykit.CandidateSet, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.CandidateSet{}, err
	}
	if err := memorykit.ValidateCandidateQuery(query, s.cfg.Limits); err != nil {
		return memorykit.CandidateSet{}, err
	}
	if query.VectorLimit > 0 &&
		(query.EmbeddingProfileID != s.cfg.EmbeddingProfileID || query.EmbeddingDimensions != s.cfg.EmbeddingDimensions) {
		return memorykit.CandidateSet{}, fmt.Errorf("%w: embedding query does not match store configuration", memorykit.ErrInvalidMemory)
	}

	var result memorykit.CandidateSet
	var err error
	if query.ExactLimit > 0 {
		result.Exact, err = s.searchExact(ctx, query)
		if err != nil {
			return memorykit.CandidateSet{}, err
		}
	}
	if query.FullTextLimit > 0 {
		result.FullText, err = s.searchFullText(ctx, query)
		if err != nil {
			return memorykit.CandidateSet{}, err
		}
	}

	var vectorErr error
	if query.VectorLimit > 0 {
		result.Vector, err = s.searchVector(ctx, query)
		if err != nil {
			result, vectorErr = vectorChannelResult(result, err)
			if vectorErr == nil {
				return memorykit.CandidateSet{}, memorykit.ErrInvalidRecallResult
			}
			if !memorykit.IsRecoverable(vectorErr) {
				return memorykit.CandidateSet{}, vectorErr
			}
		}
	}

	if err := s.attachCandidateSources(ctx, query.Scope, &result); err != nil {
		return memorykit.CandidateSet{}, err
	}
	return copyCandidateSet(result), vectorErr
}

func (s *Store) searchExact(ctx context.Context, query memorykit.CandidateQuery) ([]memorykit.Candidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+qualifiedMemoryColumns+`
		FROM memories m
		WHERE m.tenant_id = $1 AND m.subject_type = $2 AND m.subject_id = $3
		  AND m.status = 'active'
		  AND m.valid_from <= $4
		  AND (m.valid_until IS NULL OR m.valid_until > $4)
		  AND m.memory_key = ANY($5::text[])
		  AND (cardinality($6::text[]) = 0 OR m.kind = ANY($6::text[]))
		ORDER BY m.importance DESC, m.updated_at DESC, m.id
		LIMIT $7`,
		query.Scope.TenantID, query.Scope.SubjectType, query.Scope.SubjectID, query.Now,
		append([]string(nil), query.Keys...), kindStrings(query.Kinds), query.ExactLimit)
	if err != nil {
		return nil, wrapBackend("exact candidates", err)
	}
	return scanCandidates(rows, memorykit.ChannelExact, false)
}

func (s *Store) searchFullText(ctx context.Context, query memorykit.CandidateQuery) ([]memorykit.Candidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+qualifiedMemoryColumns+`
		FROM memories m
		WHERE m.tenant_id = $1 AND m.subject_type = $2 AND m.subject_id = $3
		  AND m.status = 'active'
		  AND m.valid_from <= $4
		  AND (m.valid_until IS NULL OR m.valid_until > $4)
		  AND m.search_vector @@ websearch_to_tsquery('simple', $5)
		  AND (cardinality($6::text[]) = 0 OR m.kind = ANY($6::text[]))
		ORDER BY ts_rank_cd(m.search_vector, websearch_to_tsquery('simple', $5)) DESC,
		         m.importance DESC, m.updated_at DESC, m.id ASC
		LIMIT $7`,
		query.Scope.TenantID, query.Scope.SubjectType, query.Scope.SubjectID, query.Now,
		query.Text, kindStrings(query.Kinds), query.FullTextLimit)
	if err != nil {
		return nil, wrapBackend("full-text candidates", err)
	}
	return scanCandidates(rows, memorykit.ChannelFullText, false)
}

func (s *Store) searchVector(ctx context.Context, query memorykit.CandidateQuery) ([]memorykit.Candidate, error) {
	vector := pgvector.NewVector(append([]float32(nil), query.QueryVector...))
	rows, err := s.db.QueryContext(ctx, `SELECT `+qualifiedMemoryColumns+`,
		       1 - (e.embedding <=> $7) AS similarity
		FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id
		WHERE m.tenant_id = $1 AND m.subject_type = $2 AND m.subject_id = $3
		  AND m.status = 'active'
		  AND m.valid_from <= $4
		  AND (m.valid_until IS NULL OR m.valid_until > $4)
		  AND e.embedding_profile_id = $5
		  AND e.dimension = $6
		  AND e.content_hash = m.content_hash
		  AND 1 - (e.embedding <=> $7) >= $8
		  AND (cardinality($10::text[]) = 0 OR m.kind = ANY($10::text[]))
		ORDER BY e.embedding <=> $7, m.importance DESC, m.updated_at DESC, m.id
		LIMIT $9`,
		query.Scope.TenantID, query.Scope.SubjectType, query.Scope.SubjectID, query.Now,
		query.EmbeddingProfileID, query.EmbeddingDimensions, vector,
		query.MinVectorSimilarity, query.VectorLimit, kindStrings(query.Kinds))
	if err != nil {
		return nil, wrapBackend("vector candidates", err)
	}
	return scanCandidates(rows, memorykit.ChannelVector, true)
}

func scanCandidates(rows *sql.Rows, channel memorykit.Channel, withSimilarity bool) ([]memorykit.Candidate, error) {
	defer rows.Close()
	candidates := make([]memorykit.Candidate, 0)
	for rows.Next() {
		candidate := memorykit.Candidate{Channel: channel, Rank: len(candidates) + 1}
		var err error
		if withSimilarity {
			candidate.Memory, err = scanMemoryWithTail(rows, &candidate.Similarity)
		} else {
			candidate.Memory, err = scanMemory(rows)
		}
		if err != nil {
			return nil, wrapBackend("scan candidates", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapBackend("read candidates", err)
	}
	return candidates, nil
}

func vectorChannelResult(partial memorykit.CandidateSet, err error) (memorykit.CandidateSet, error) {
	if err == nil {
		return partial, nil
	}
	if !memorykit.IsRecoverable(err) {
		return memorykit.CandidateSet{}, err
	}
	partial.Vector = nil
	return partial, &memorykit.ChannelError{Channel: memorykit.ChannelVector, Err: err}
}

func (s *Store) attachCandidateSources(ctx context.Context, scope memorykit.Scope, set *memorykit.CandidateSet) error {
	ids := candidateIDs(*set)
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.memory_id, s.source_kind, s.source_ref, s.evidence_hash
		FROM memory_sources s
		JOIN memories m ON m.id = s.memory_id
		WHERE m.tenant_id = $1 AND m.subject_type = $2 AND m.subject_id = $3
		  AND s.memory_id = ANY($4::uuid[])
		ORDER BY s.memory_id, s.source_ref, s.source_kind`,
		scope.TenantID, scope.SubjectType, scope.SubjectID, ids)
	if err != nil {
		return wrapBackend("candidate sources", err)
	}
	defer rows.Close()
	sources := make(map[string][]memorykit.Source, len(ids))
	for rows.Next() {
		var memoryID string
		var source memorykit.Source
		if err := rows.Scan(&memoryID, &source.Kind, &source.Ref, &source.EvidenceHash); err != nil {
			return wrapBackend("scan candidate sources", err)
		}
		sources[memoryID] = append(sources[memoryID], source)
	}
	if err := rows.Err(); err != nil {
		return wrapBackend("read candidate sources", err)
	}
	attach := func(candidates []memorykit.Candidate) {
		for index := range candidates {
			candidates[index].Sources = append([]memorykit.Source(nil), sources[candidates[index].Memory.ID]...)
		}
	}
	attach(set.Exact)
	attach(set.FullText)
	attach(set.Vector)
	return nil
}

func candidateIDs(set memorykit.CandidateSet) []string {
	total := len(set.Exact) + len(set.FullText) + len(set.Vector)
	seen := make(map[string]struct{}, total)
	ids := make([]string, 0, total)
	for _, candidates := range [][]memorykit.Candidate{set.Exact, set.FullText, set.Vector} {
		for _, candidate := range candidates {
			if _, exists := seen[candidate.Memory.ID]; exists {
				continue
			}
			seen[candidate.Memory.ID] = struct{}{}
			ids = append(ids, candidate.Memory.ID)
		}
	}
	return ids
}

func kindStrings(kinds []memorykit.Kind) []string {
	result := make([]string, len(kinds))
	for index := range kinds {
		result[index] = string(kinds[index])
	}
	return result
}

func copyCandidateSet(set memorykit.CandidateSet) memorykit.CandidateSet {
	copyCandidates := func(candidates []memorykit.Candidate) []memorykit.Candidate {
		result := make([]memorykit.Candidate, len(candidates))
		copy(result, candidates)
		for index := range result {
			result[index].Sources = append([]memorykit.Source(nil), candidates[index].Sources...)
		}
		return result
	}
	return memorykit.CandidateSet{
		Exact: copyCandidates(set.Exact), FullText: copyCandidates(set.FullText), Vector: copyCandidates(set.Vector),
	}
}
