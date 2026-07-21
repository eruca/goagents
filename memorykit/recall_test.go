package memorykit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

func TestRecallerFusesRanksAndPacksBudget(t *testing.T) {
	store := &fakeRecallStore{set: CandidateSet{
		Exact:    []Candidate{{Memory: memory("m-exact", 20), Rank: 1, Channel: ChannelExact}},
		FullText: []Candidate{{Memory: memory("m-shared", 50), Rank: 1, Channel: ChannelFullText}},
		Vector: []Candidate{
			{Memory: memory("m-shared", 50), Rank: 1, Channel: ChannelVector, Similarity: 0.91},
			{Memory: memory("m-vector", 30), Rank: 2, Channel: ChannelVector, Similarity: 0.88},
		},
	}}
	recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, testRecallPolicy())

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Text: "how do we test",
		Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := recallItemIDs(got.Items); !reflect.DeepEqual(ids, []string{"m-shared", "m-exact"}) {
		t.Fatalf("ids = %v", ids)
	}
	if got.PolicyVersion != "test-v1" {
		t.Fatalf("PolicyVersion = %q", got.PolicyVersion)
	}
}

func TestRecallPolicyAndConstructorRejectMissingConfiguration(t *testing.T) {
	valid := testRecallPolicy()
	tests := []struct {
		name   string
		mutate func(*RecallPolicy)
	}{
		{name: "version", mutate: func(policy *RecallPolicy) { policy.Version = "" }},
		{name: "channel", mutate: func(policy *RecallPolicy) { policy.ExactLimit, policy.FullTextLimit, policy.VectorLimit = 0, 0, 0 }},
		{name: "rrf", mutate: func(policy *RecallPolicy) { policy.RRFK = 0 }},
		{name: "similarity", mutate: func(policy *RecallPolicy) { policy.MinVectorSimilarity = 2 }},
		{name: "items", mutate: func(policy *RecallPolicy) { policy.MaxItems = 0 }},
		{name: "tokens", mutate: func(policy *RecallPolicy) { policy.MaxTokens = 0 }},
		{name: "query runes", mutate: func(policy *RecallPolicy) { policy.MaxQueryRunes = 0 }},
		{name: "keys", mutate: func(policy *RecallPolicy) { policy.MaxKeys = 0 }},
		{name: "deadline", mutate: func(policy *RecallPolicy) { policy.Deadline = 0 }},
		{name: "embedding profile", mutate: func(policy *RecallPolicy) { policy.EmbeddingProfileID = "" }},
		{name: "embedding dimensions", mutate: func(policy *RecallPolicy) { policy.EmbeddingDimensions = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := valid
			test.mutate(&policy)
			if err := policy.Validate(); err == nil {
				t.Fatal("Validate accepted invalid policy")
			}
		})
	}

	store := &fakeRecallStore{}
	embedder := fixedEmbedder{vector: []float32{1, 0, 0}}
	counter := TokenCounter(func(string) int { return 1 })
	configs := []RecallConfig{
		{Embedder: embedder, CountTokens: counter, Policy: valid},
		{Store: store, CountTokens: counter, Policy: valid},
		{Store: store, Embedder: embedder, Policy: valid},
	}
	for index, config := range configs {
		if _, err := NewRecaller(config); err == nil {
			t.Fatalf("NewRecaller accepted missing dependency at index %d", index)
		}
	}
}

func TestRecallerDegradesOnlyTypedRecoverableVectorFailures(t *testing.T) {
	recoverable := &BackendError{Op: "embed", Recoverable: true, Err: errors.New("provider unavailable for secret query")}
	store := &fakeRecallStore{set: CandidateSet{
		Exact: []Candidate{{Memory: memory("m-exact", 20), Rank: 1, Channel: ChannelExact}},
	}}
	recaller := newTestRecaller(t, store, fixedEmbedder{err: recoverable}, testRecallPolicy())

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Text: "secret query",
		Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := recallItemIDs(got.Items); !reflect.DeepEqual(ids, []string{"m-exact"}) {
		t.Fatalf("ids = %v", ids)
	}
	if !reflect.DeepEqual(got.DegradedChannels, []Channel{ChannelVector}) {
		t.Fatalf("DegradedChannels = %v", got.DegradedChannels)
	}
	if store.calls != 1 || store.queries[0].VectorLimit != 0 || len(store.queries[0].QueryVector) != 0 ||
		store.queries[0].EmbeddingProfileID != "" || store.queries[0].EmbeddingDimensions != 0 {
		t.Fatalf("degraded query = %#v", store.queries[0])
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret query") || strings.Contains(string(encoded), "provider unavailable") {
		t.Fatalf("degraded result leaked sensitive text: %s", encoded)
	}
}

func TestRecallerReturnsEmptyDegradedResultWhenVectorWasOnlyRunnableChannel(t *testing.T) {
	store := &fakeRecallStore{}
	policy := testRecallPolicy()
	policy.ExactLimit = 0
	policy.FullTextLimit = 0
	recaller := newTestRecaller(t, store, fixedEmbedder{err: &BackendError{
		Op: "embed", Recoverable: true, Err: errors.New("offline"),
	}}, policy)

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Text: "query",
		Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 0 || !reflect.DeepEqual(got.DegradedChannels, []Channel{ChannelVector}) {
		t.Fatalf("result = %#v", got)
	}
	if store.calls != 0 {
		t.Fatalf("store received an invalid zero-channel query")
	}
}

func TestRecallerKeepsPartialResultsForTypedRecoverableVectorStoreFailure(t *testing.T) {
	store := &fakeRecallStore{
		set: CandidateSet{Exact: []Candidate{{Memory: memory("m-exact", 20), Rank: 1, Channel: ChannelExact}}},
		err: &ChannelError{Channel: ChannelVector, Err: &BackendError{
			Op: "vector", Recoverable: true, Err: errors.New("temporary vector failure"),
		}},
	}
	recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, testRecallPolicy())

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Text: "query",
		Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := recallItemIDs(got.Items); !reflect.DeepEqual(ids, []string{"m-exact"}) {
		t.Fatalf("ids = %v", ids)
	}
	if !reflect.DeepEqual(got.DegradedChannels, []Channel{ChannelVector}) {
		t.Fatalf("DegradedChannels = %v", got.DegradedChannels)
	}
}

func TestRecallerRedactsStoreErrorsAndPreservesClassification(t *testing.T) {
	secret := "query-sentinel content-sentinel vector-sentinel"
	tests := []struct {
		name        string
		err         error
		recoverable bool
		channel     Channel
	}{
		{name: "non recoverable", err: errors.New(secret)},
		{name: "recoverable backend without channel", recoverable: true, err: &BackendError{
			Op: secret, Recoverable: true, Err: errors.New(secret),
		}},
		{name: "recoverable full text channel", recoverable: true, channel: ChannelFullText, err: &ChannelError{
			Channel: ChannelFullText, Err: &BackendError{Op: secret, Recoverable: true, Err: errors.New(secret)},
		}},
		{name: "non recoverable exact channel", channel: ChannelExact, err: &ChannelError{
			Channel: ChannelExact, Err: errors.New(secret),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeRecallStore{err: test.err}
			recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, testRecallPolicy())
			_, err := recaller.Recall(context.Background(), RecallRequest{
				Scope: projectScope("project-1"), Text: "query", Now: fixedNow,
			})
			assertSafeDependencyError(t, err, test.err, test.recoverable)
			var channelErr *ChannelError
			if test.channel == "" && errors.As(err, &channelErr) {
				t.Fatalf("unexpected channel error: %v", err)
			}
			if test.channel != "" && (!errors.As(err, &channelErr) || channelErr.Channel != test.channel) {
				t.Fatalf("channel error = %#v", channelErr)
			}
		})
	}
}

func TestRecallerRedactsNonRecoverableEmbeddingError(t *testing.T) {
	want := errors.New("query-sentinel content-sentinel vector-sentinel")
	store := &fakeRecallStore{}
	recaller := newTestRecaller(t, store, fixedEmbedder{err: want}, testRecallPolicy())

	_, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Text: "query", Now: fixedNow,
	})
	assertSafeDependencyError(t, err, want, false)
	if store.calls != 0 {
		t.Fatalf("store calls = %d", store.calls)
	}
}

func TestRecallerPreservesDependencyContextErrors(t *testing.T) {
	t.Run("embedder canceled", func(t *testing.T) {
		recaller := newTestRecaller(t, &fakeRecallStore{}, fixedEmbedder{
			err: fmt.Errorf("secret wrapper: %w", context.Canceled),
		}, testRecallPolicy())
		_, err := recaller.Recall(context.Background(), RecallRequest{
			Scope: projectScope("project-1"), Text: "query", Now: fixedNow,
		})
		if err != context.Canceled {
			t.Fatalf("err = %#v", err)
		}
	})

	t.Run("store deadline", func(t *testing.T) {
		store := &fakeRecallStore{err: fmt.Errorf("secret wrapper: %w", context.DeadlineExceeded)}
		recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, testRecallPolicy())
		_, err := recaller.Recall(context.Background(), RecallRequest{
			Scope: projectScope("project-1"), Text: "query", Now: fixedNow,
		})
		if err != context.DeadlineExceeded {
			t.Fatalf("err = %#v", err)
		}
	})
}

func TestRecallerDefensivelyFiltersStatusAndEffectiveTime(t *testing.T) {
	valid := memory("valid", 10)
	candidate := memory("candidate", 100)
	candidate.Status = StatusCandidate
	inactive := memory("inactive", 100)
	inactive.Status = StatusInactive
	expired := memory("expired", 100)
	expired.ValidUntil = fixedNow
	future := memory("future", 100)
	future.ValidFrom = fixedNow.Add(time.Minute)
	store := &fakeRecallStore{set: CandidateSet{Exact: []Candidate{
		{Memory: candidate, Rank: 1, Channel: ChannelExact},
		{Memory: inactive, Rank: 2, Channel: ChannelExact},
		{Memory: expired, Rank: 3, Channel: ChannelExact},
		{Memory: future, Rank: 4, Channel: ChannelExact},
		{Memory: valid, Rank: 5, Channel: ChannelExact},
	}}}
	policy := testRecallPolicy()
	policy.ExactLimit = 8
	recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, policy)

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := recallItemIDs(got.Items); !reflect.DeepEqual(ids, []string{"valid"}) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestRecallerFailsClosedOnInvalidStoreResponseShape(t *testing.T) {
	valid := Candidate{Memory: memory("valid", 10), Rank: 1, Channel: ChannelExact}
	tests := []struct {
		name    string
		request RecallRequest
		policy  RecallPolicy
		set     CandidateSet
	}{
		{
			name:    "channel does not match container",
			request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow},
			policy:  testRecallPolicy(), set: CandidateSet{Exact: []Candidate{{Memory: valid.Memory, Rank: 1, Channel: ChannelFullText}}},
		},
		{
			name:    "exact result without keys",
			request: RecallRequest{Scope: projectScope("project-1"), Text: "query", Now: fixedNow},
			policy:  testRecallPolicy(), set: CandidateSet{Exact: []Candidate{valid}},
		},
		{
			name:    "full text result without text",
			request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow},
			policy:  testRecallPolicy(), set: CandidateSet{FullText: []Candidate{{Memory: valid.Memory, Rank: 1, Channel: ChannelFullText}}},
		},
		{
			name:    "vector result without query vector",
			request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow},
			policy:  testRecallPolicy(), set: CandidateSet{Vector: []Candidate{{Memory: valid.Memory, Rank: 1, Channel: ChannelVector, Similarity: 0.9}}},
		},
		{
			name:    "slice exceeds limit",
			request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow},
			policy:  func() RecallPolicy { policy := testRecallPolicy(); policy.ExactLimit = 1; return policy }(),
			set:     CandidateSet{Exact: []Candidate{valid, {Memory: memory("second", 9), Rank: 1, Channel: ChannelExact}}},
		},
		{
			name:    "rank exceeds limit",
			request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow},
			policy:  func() RecallPolicy { policy := testRecallPolicy(); policy.ExactLimit = 1; return policy }(),
			set:     CandidateSet{Exact: []Candidate{{Memory: valid.Memory, Rank: 2, Channel: ChannelExact}}},
		},
		{
			name:    "duplicate ID in channel",
			request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow},
			policy:  testRecallPolicy(), set: CandidateSet{Exact: []Candidate{valid, valid}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertInvalidRecallSet(t, test.policy, test.request, test.set, nil)
		})
	}
}

func TestRecallerFailsClosedOnMalformedStoreMemoryAndSources(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Candidate)
	}{
		{name: "blank ID", mutate: func(candidate *Candidate) { candidate.Memory.ID = "" }},
		{name: "wrong scope", mutate: func(candidate *Candidate) { candidate.Memory.Scope = projectScope("project-2") }},
		{name: "invalid kind", mutate: func(candidate *Candidate) { candidate.Memory.Kind = Kind("invalid") }},
		{name: "importance", mutate: func(candidate *Candidate) { candidate.Memory.Importance = 101 }},
		{name: "confidence NaN", mutate: func(candidate *Candidate) { candidate.Memory.Confidence = math.NaN() }},
		{name: "confidence range", mutate: func(candidate *Candidate) { candidate.Memory.Confidence = 2 }},
		{name: "version", mutate: func(candidate *Candidate) { candidate.Memory.Version = 0 }},
		{name: "source kind", mutate: func(candidate *Candidate) { candidate.Sources = []Source{{Ref: "ref"}} }},
		{name: "source ref", mutate: func(candidate *Candidate) { candidate.Sources = []Source{{Kind: "event"}} }},
	}
	request := RecallRequest{Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := Candidate{Memory: memory("invalid", 10), Rank: 1, Channel: ChannelExact}
			test.mutate(&candidate)
			assertInvalidRecallSet(t, testRecallPolicy(), request, CandidateSet{Exact: []Candidate{candidate}}, nil)
		})
	}
}

func TestRecallerFailsClosedOnCrossChannelCorruption(t *testing.T) {
	request := RecallRequest{
		Scope: projectScope("project-1"), Text: "query", Keys: []string{"build.test_command"}, Now: fixedNow,
	}
	base := memory("same", 10)

	t.Run("memory mismatch", func(t *testing.T) {
		changed := base
		changed.Content = "different content"
		assertInvalidRecallSet(t, testRecallPolicy(), request, CandidateSet{
			Exact:  []Candidate{{Memory: base, Rank: 1, Channel: ChannelExact}},
			Vector: []Candidate{{Memory: changed, Rank: 1, Channel: ChannelVector, Similarity: 0.9}},
		}, nil)
	})

	t.Run("normalized source refs mismatch", func(t *testing.T) {
		assertInvalidRecallSet(t, testRecallPolicy(), request, CandidateSet{
			Exact:  []Candidate{{Memory: base, Sources: []Source{{Kind: "event", Ref: "a"}}, Rank: 1, Channel: ChannelExact}},
			Vector: []Candidate{{Memory: base, Sources: []Source{{Kind: "event", Ref: "b"}}, Rank: 1, Channel: ChannelVector, Similarity: 0.9}},
		}, nil)
	})

	t.Run("filtered status still must match", func(t *testing.T) {
		changed := base
		changed.Status = StatusInactive
		assertInvalidRecallSet(t, testRecallPolicy(), request, CandidateSet{
			Exact:  []Candidate{{Memory: base, Rank: 1, Channel: ChannelExact}},
			Vector: []Candidate{{Memory: changed, Rank: 1, Channel: ChannelVector, Similarity: 0.9}},
		}, nil)
	})
}

func TestRecallerAcceptsEquivalentNormalizedSourceRefsAcrossChannels(t *testing.T) {
	shared := memory("same", 10)
	store := &fakeRecallStore{set: CandidateSet{
		Exact: []Candidate{{
			Memory:  shared,
			Sources: []Source{{Kind: "event", Ref: "b"}, {Kind: "event", Ref: "a"}, {Kind: "duplicate", Ref: "b"}},
			Rank:    1, Channel: ChannelExact,
		}},
		Vector: []Candidate{{
			Memory: shared, Sources: []Source{{Kind: "event", Ref: "a"}, {Kind: "event", Ref: "b"}},
			Rank: 1, Channel: ChannelVector, Similarity: 0.9,
		}},
	}}
	recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, testRecallPolicy())
	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Text: "query", Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := recallItemIDs(got.Items); !reflect.DeepEqual(ids, []string{"same"}) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestRecallerFailsClosedOnInvalidRecoverableVectorChannelError(t *testing.T) {
	recoverableVector := &ChannelError{Channel: ChannelVector, Err: &BackendError{
		Op: "vector", Recoverable: true, Err: errors.New("offline"),
	}}

	t.Run("vector was disabled", func(t *testing.T) {
		assertInvalidRecallSet(t, testRecallPolicy(), RecallRequest{
			Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow,
		}, CandidateSet{}, recoverableVector)
	})

	t.Run("partial vector rows", func(t *testing.T) {
		assertInvalidRecallSet(t, testRecallPolicy(), RecallRequest{
			Scope: projectScope("project-1"), Text: "query", Now: fixedNow,
		}, CandidateSet{Vector: []Candidate{{
			Memory: memory("partial", 10), Rank: 1, Channel: ChannelVector, Similarity: 0.9,
		}}}, recoverableVector)
	})
}

func TestRecallerOrdersTiesByImportanceUpdatedAtAndID(t *testing.T) {
	important := memory("important", 60)
	newer := memory("newer", 50)
	newer.UpdatedAt = fixedNow.Add(time.Minute)
	idA := memory("a", 50)
	idB := memory("b", 50)
	store := &fakeRecallStore{set: CandidateSet{Exact: []Candidate{
		{Memory: idB, Rank: 1, Channel: ChannelExact},
		{Memory: newer, Rank: 1, Channel: ChannelExact},
		{Memory: idA, Rank: 1, Channel: ChannelExact},
		{Memory: important, Rank: 1, Channel: ChannelExact},
	}}}
	policy := testRecallPolicy()
	policy.MaxItems = 4
	policy.MaxTokens = 100
	recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, policy)

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := recallItemIDs(got.Items); !reflect.DeepEqual(ids, []string{"important", "newer", "a", "b"}) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestRecallerCountsCanonicalRecordAndSkipsOversizedHigherRank(t *testing.T) {
	large := memory("large", 100)
	large.Key = "key-with-many-budget-characters"
	small := memory("small", 10)
	small.Key = "k"
	largeSources := []Source{
		{Kind: "event", Ref: "z-ref", EvidenceHash: "hash-1"},
		{Kind: "other", Ref: "a-ref", EvidenceHash: "hash-2"},
		{Kind: "duplicate", Ref: "z-ref", EvidenceHash: "hash-3"},
	}
	store := &fakeRecallStore{set: CandidateSet{Exact: []Candidate{
		{Memory: large, Sources: largeSources, Rank: 1, Channel: ChannelExact},
		{Memory: small, Rank: 2, Channel: ChannelExact},
	}}}
	var payloads []string
	policy := testRecallPolicy()
	policy.MaxItems = 2
	policy.MaxTokens = 120
	recaller, err := NewRecaller(RecallConfig{
		Store: store, Embedder: fixedEmbedder{vector: []float32{1, 0, 0}}, Policy: policy,
		CountTokens: func(payload string) int {
			payloads = append(payloads, payload)
			return len(payload)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := recallItemIDs(got.Items); !reflect.DeepEqual(ids, []string{"small"}) {
		t.Fatalf("ids = %v", ids)
	}
	if len(payloads) != 2 {
		t.Fatalf("payload count = %d", len(payloads))
	}
	wantLarge := `{"id":"large","kind":"decision","key":"key-with-many-budget-characters","content":"Run the verified project command","source_refs":["a-ref","z-ref"]}`
	if payloads[0] != wantLarge {
		t.Fatalf("large payload = %s", payloads[0])
	}
	wantSmall := `{"id":"small","kind":"decision","key":"k","content":"Run the verified project command","source_refs":[]}`
	if payloads[1] != wantSmall {
		t.Fatalf("small payload = %s", payloads[1])
	}
}

func TestRecallerEnforcesItemBudget(t *testing.T) {
	store := &fakeRecallStore{set: CandidateSet{Exact: []Candidate{
		{Memory: memory("first", 30), Rank: 1, Channel: ChannelExact},
		{Memory: memory("second", 20), Rank: 2, Channel: ChannelExact},
	}}}
	policy := testRecallPolicy()
	policy.MaxItems = 1
	recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, policy)

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := recallItemIDs(got.Items); !reflect.DeepEqual(ids, []string{"first"}) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestRecallerRejectsInvalidRequestsBeforeDependencies(t *testing.T) {
	tests := []struct {
		name    string
		request RecallRequest
	}{
		{name: "scope", request: RecallRequest{Text: "query", Now: fixedNow}},
		{name: "blank query", request: RecallRequest{Scope: projectScope("project-1"), Text: "  ", Now: fixedNow}},
		{name: "long query", request: RecallRequest{Scope: projectScope("project-1"), Text: strings.Repeat("x", 257), Now: fixedNow}},
		{name: "too many keys", request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}, Now: fixedNow}},
		{name: "blank key", request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{" "}, Now: fixedNow}},
		{name: "duplicate key", request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"key", "key"}, Now: fixedNow}},
		{name: "invalid kind", request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"key"}, Kinds: []Kind{"unknown"}, Now: fixedNow}},
		{name: "duplicate kind", request: RecallRequest{Scope: projectScope("project-1"), Keys: []string{"key"}, Kinds: []Kind{KindFact, KindFact}, Now: fixedNow}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeRecallStore{}
			embedder := &recordingEmbedder{vector: []float32{1, 0, 0}}
			recaller := newTestRecaller(t, store, embedder, testRecallPolicy())
			if _, err := recaller.Recall(context.Background(), test.request); err == nil {
				t.Fatal("Recall accepted invalid request")
			}
			if store.calls != 0 || embedder.calls != 0 {
				t.Fatalf("calls = store %d embedder %d", store.calls, embedder.calls)
			}
		})
	}
}

func TestRecallerUsesOneDeadlineAndCopiesInputsAndResults(t *testing.T) {
	keys := []string{"build.test_command"}
	kinds := []Kind{KindDecision}
	vector := []float32{1, 0, 0}
	sources := []Source{{Kind: "event", Ref: "source-1", EvidenceHash: "hash"}}
	embedder := &recordingEmbedder{vector: vector}
	store := &fakeRecallStore{set: CandidateSet{Exact: []Candidate{{
		Memory: memory("copied", 10), Sources: sources, Rank: 1, Channel: ChannelExact,
	}}}}
	store.hook = func(ctx context.Context, query CandidateQuery) {
		store.deadline, _ = contextDeadline(ctx)
		query.Keys[0] = "mutated"
		query.Kinds[0] = KindFact
		query.QueryVector[0] = 9
	}
	recaller := newTestRecaller(t, store, embedder, testRecallPolicy())

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Text: "query", Keys: keys, Kinds: kinds, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !embedder.hasDeadline || store.deadline.IsZero() || !embedder.deadline.Equal(store.deadline) {
		t.Fatalf("deadlines = embedder %v store %v", embedder.deadline, store.deadline)
	}
	if keys[0] != "build.test_command" || kinds[0] != KindDecision || vector[0] != 1 {
		t.Fatalf("caller input mutated: keys=%v kinds=%v vector=%v", keys, kinds, vector)
	}
	got.Items[0].Sources[0].Ref = "changed"
	if store.set.Exact[0].Sources[0].Ref != "source-1" {
		t.Fatalf("store result source mutated: %#v", store.set.Exact[0].Sources)
	}
}

func TestRecallerEnforcesDeadlineWhenDependenciesIgnoreCancellation(t *testing.T) {
	t.Run("embedder", func(t *testing.T) {
		store := &fakeRecallStore{}
		policy := testRecallPolicy()
		policy.Deadline = 5 * time.Millisecond
		recaller := newTestRecaller(t, store, deadlineIgnoringEmbedder{}, policy)

		_, err := recaller.Recall(context.Background(), RecallRequest{
			Scope: projectScope("project-1"), Text: "query", Now: fixedNow,
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		if store.calls != 0 {
			t.Fatalf("store calls = %d", store.calls)
		}
	})

	t.Run("store", func(t *testing.T) {
		store := &fakeRecallStore{}
		store.hook = func(ctx context.Context, _ CandidateQuery) { <-ctx.Done() }
		policy := testRecallPolicy()
		policy.Deadline = 5 * time.Millisecond
		recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, policy)

		_, err := recaller.Recall(context.Background(), RecallRequest{
			Scope: projectScope("project-1"), Text: "query", Now: fixedNow,
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRecallerDeadlineCoversTokenBudgetPacking(t *testing.T) {
	store := &fakeRecallStore{set: CandidateSet{Exact: []Candidate{{
		Memory: memory("late-item", 10), Rank: 1, Channel: ChannelExact,
	}}}}
	policy := testRecallPolicy()
	policy.Deadline = 10 * time.Millisecond
	recaller, err := NewRecaller(RecallConfig{
		Store: store, Embedder: fixedEmbedder{vector: []float32{1, 0, 0}}, Policy: policy,
		CountTokens: func(string) int {
			time.Sleep(30 * time.Millisecond)
			return 1
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := recaller.Recall(context.Background(), RecallRequest{
		Scope: projectScope("project-1"), Keys: []string{"build.test_command"}, Now: fixedNow,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, result = %#v", err, got)
	}
}

func TestRecallerRejectsInvalidEmbeddingResponseBeforeStore(t *testing.T) {
	tests := []struct {
		name   string
		vector []float32
	}{
		{name: "wrong dimensions", vector: []float32{1, 0}},
		{name: "non finite", vector: []float32{1, 0, float32(math.Inf(1))}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeRecallStore{}
			recaller := newTestRecaller(t, store, fixedEmbedder{vector: test.vector}, testRecallPolicy())
			if _, err := recaller.Recall(context.Background(), RecallRequest{
				Scope: projectScope("project-1"), Text: "query", Now: fixedNow,
			}); err == nil {
				t.Fatal("Recall accepted invalid embedding response")
			}
			if store.calls != 0 {
				t.Fatalf("store calls = %d", store.calls)
			}
		})
	}
}

func projectScope(project string) Scope {
	return Scope{TenantID: "tenant-1", SubjectType: SubjectProject, SubjectID: project}
}

func memory(id string, importance int) Memory {
	return Memory{
		ID: id, Scope: projectScope("project-1"), Kind: KindDecision,
		Key: "build.test_command", Status: StatusActive,
		Content:   "Run the verified project command",
		ValidFrom: fixedNow.Add(-time.Hour), Importance: importance,
		Version: 1, CreatedAt: fixedNow.Add(-time.Hour), UpdatedAt: fixedNow,
	}
}

func testRecallPolicy() RecallPolicy {
	return RecallPolicy{
		Version: "test-v1", ExactLimit: 4, FullTextLimit: 4, VectorLimit: 4,
		RRFK: 60, MinVectorSimilarity: 0.7, MaxItems: 2, MaxTokens: 12,
		MaxQueryRunes: 256, MaxKeys: 8, Deadline: time.Second,
		EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	}
}

func newTestRecaller(t *testing.T, store RecallStore, embedder Embedder, policy RecallPolicy) *Recaller {
	t.Helper()
	recaller, err := NewRecaller(RecallConfig{
		Store: store, Embedder: embedder,
		CountTokens: func(string) int { return 1 },
		Policy:      policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	return recaller
}

func assertInvalidRecallSet(t *testing.T, policy RecallPolicy, request RecallRequest, set CandidateSet, storeErr error) {
	t.Helper()
	store := &fakeRecallStore{set: set, err: storeErr}
	recaller := newTestRecaller(t, store, fixedEmbedder{vector: []float32{1, 0, 0}}, policy)
	_, err := recaller.Recall(context.Background(), request)
	if !errors.Is(err, ErrInvalidRecallResult) {
		t.Fatalf("err = %v, want ErrInvalidRecallResult", err)
	}
}

func assertSafeDependencyError(t *testing.T, err, original error, recoverable bool) {
	t.Helper()
	if err == nil {
		t.Fatal("expected dependency error")
	}
	for _, secret := range []string{"query-sentinel", "content-sentinel", "vector-sentinel"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
	if errors.Is(err, original) {
		t.Fatalf("error retained raw dependency cause: %v", err)
	}
	if !errors.Is(err, ErrRecallDependency) {
		t.Fatalf("err = %v, want ErrRecallDependency", err)
	}
	if IsRecoverable(err) != recoverable {
		t.Fatalf("IsRecoverable(%v) = %v", err, IsRecoverable(err))
	}
}

func recallItemIDs(items []RecallItem) []string {
	ids := make([]string, len(items))
	for index, item := range items {
		ids[index] = item.Memory.ID
	}
	return ids
}

type fakeRecallStore struct {
	set      CandidateSet
	err      error
	calls    int
	queries  []CandidateQuery
	hook     func(context.Context, CandidateQuery)
	deadline time.Time
}

func (s *fakeRecallStore) SearchCandidates(ctx context.Context, query CandidateQuery) (CandidateSet, error) {
	s.calls++
	s.queries = append(s.queries, query)
	if s.hook != nil {
		s.hook(ctx, query)
	}
	return s.set, s.err
}

type fixedEmbedder struct {
	vector []float32
	err    error
}

func (e fixedEmbedder) Embed(context.Context, EmbedRequest) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	return [][]float32{append([]float32(nil), e.vector...)}, nil
}

type recordingEmbedder struct {
	vector      []float32
	calls       int
	deadline    time.Time
	hasDeadline bool
}

func (e *recordingEmbedder) Embed(ctx context.Context, _ EmbedRequest) ([][]float32, error) {
	e.calls++
	e.deadline, e.hasDeadline = ctx.Deadline()
	return [][]float32{append([]float32(nil), e.vector...)}, nil
}

type deadlineIgnoringEmbedder struct{}

func (deadlineIgnoringEmbedder) Embed(ctx context.Context, _ EmbedRequest) ([][]float32, error) {
	<-ctx.Done()
	return [][]float32{{1, 0, 0}}, nil
}

func contextDeadline(ctx context.Context) (time.Time, bool) { return ctx.Deadline() }
