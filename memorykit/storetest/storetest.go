package storetest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
)

func RunLifecycleConformance(t *testing.T, newStore func(*testing.T) memorykit.LifecycleStore) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, memorykit.LifecycleStore)
	}{
		{"candidate is never active", assertCandidateNotActive},
		{"active create supersedes same scope kind key", assertActiveCreateSupersedes},
		{"different project remains isolated", assertProjectIsolation},
		{"activate candidate atomically supersedes active", assertActivationSupersedes},
		{"sequential activation with equal time supersedes active", assertSequentialActivationWithEqualTime},
		{"stale expected version conflicts", assertStaleVersionConflicts},
		{"wrong starting status conflicts without side effects", assertWrongStartingStatusConflicts},
		{"correct increments version and preserves history", assertCorrectionHistory},
		{"dismiss makes candidate inactive", assertDismissedCandidateInactive},
		{"forget stops active reads and keeps content history", assertForgetSemantics},
		{"erase removes content sources and revision snapshots", assertEraseSemantics},
		{"returns defensive copies", assertDefensiveCopies},
		{"honors context cancellation", assertCancellation},
		{"identical idempotent replay has no side effects", assertIdempotentReplay},
		{"source order participates in idempotent replay", assertSourceOrderParticipatesInIdempotency},
		{"idempotency conflicts do not overwrite", assertIdempotencyConflicts},
		{"erase invalidates idempotent replay", assertEraseInvalidatesReplay},
		{"non canonical identity is rejected", assertNonCanonicalIdentityRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newStore(t)) })
	}
}

func baseCreate(id, project string, status memorykit.Status) memorykit.CreateRequest {
	return memorykit.CreateRequest{
		ID: id,
		Scope: memorykit.Scope{
			TenantID: "tenant-conformance", SubjectType: memorykit.SubjectProject, SubjectID: project,
		},
		Kind:       memorykit.KindDecision,
		Key:        "build.test_command",
		Status:     status,
		Content:    "Run go test ./...",
		ValidFrom:  time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
		Importance: 80,
		Confidence: 1,
		Actor:      "user-1",
		Reason:     "conformance",
		Sources:    []memorykit.Source{{Kind: "artifact", Ref: "artifact:test-command"}},
		Now:        time.Date(2026, 7, 21, 1, 0, 0, 0, time.UTC),
	}
}

func assertCandidateNotActive(t *testing.T, store memorykit.LifecycleStore) {
	created, err := store.Create(context.Background(), baseCreate("11111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusCandidate))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != memorykit.StatusCandidate {
		t.Fatalf("status = %s", created.Status)
	}
	active, err := store.List(context.Background(), memorykit.ListQuery{Scope: created.Scope, Status: memorykit.StatusActive, Limit: 10})
	if err != nil || len(active) != 0 {
		t.Fatalf("active = %#v, err = %v", active, err)
	}
}

func assertActiveCreateSupersedes(t *testing.T, store memorykit.LifecycleStore) {
	first, err := store.Create(context.Background(), baseCreate("21111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusActive))
	if err != nil {
		t.Fatal(err)
	}
	request := baseCreate("22222222-2222-2222-2222-222222222222", "project-1", memorykit.StatusActive)
	request.Content = "Run go test -race ./..."
	second, err := store.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.Get(context.Background(), first.Scope, first.ID)
	if err != nil || old.Status != memorykit.StatusInactive || second.Status != memorykit.StatusActive {
		t.Fatalf("old/new = %#v/%#v, err = %v", old, second, err)
	}
	if old.Version != first.Version+1 {
		t.Fatalf("superseded version = %d, want %d", old.Version, first.Version+1)
	}
}

func assertProjectIsolation(t *testing.T, store memorykit.LifecycleStore) {
	request := baseCreate("31111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusActive)
	if _, err := store.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	projectOne, err := store.List(context.Background(), memorykit.ListQuery{Scope: request.Scope, Status: memorykit.StatusActive, Limit: 10})
	if err != nil || len(projectOne) != 1 {
		t.Fatalf("project-1 List = %#v, %v", projectOne, err)
	}
	foreignScopes := []struct {
		name  string
		scope memorykit.Scope
	}{
		{"different project", memorykit.Scope{TenantID: request.Scope.TenantID, SubjectType: memorykit.SubjectProject, SubjectID: "project-2"}},
		{"different tenant", memorykit.Scope{TenantID: "tenant-other", SubjectType: memorykit.SubjectProject, SubjectID: request.Scope.SubjectID}},
		{"different subject type", memorykit.Scope{TenantID: request.Scope.TenantID, SubjectType: memorykit.SubjectUser, SubjectID: request.Scope.SubjectID}},
	}
	for _, foreign := range foreignScopes {
		if _, err := store.Get(context.Background(), foreign.scope, request.ID); !errors.Is(err, memorykit.ErrNotFound) {
			t.Fatalf("%s Get error = %v", foreign.name, err)
		}
		listed, err := store.List(context.Background(), memorykit.ListQuery{Scope: foreign.scope, Status: memorykit.StatusActive, Limit: 10})
		if err != nil || len(listed) != 0 {
			t.Fatalf("%s List = %#v, %v", foreign.name, listed, err)
		}
	}
}

func assertActivationSupersedes(t *testing.T, store memorykit.LifecycleStore) {
	active, err := store.Create(context.Background(), baseCreate("41111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusActive))
	if err != nil {
		t.Fatal(err)
	}
	candidateRequest := baseCreate("42222222-2222-2222-2222-222222222222", "project-1", memorykit.StatusCandidate)
	candidateRequest.Content = "Use the pgvector integration gate"
	candidate, err := store.Create(context.Background(), candidateRequest)
	if err != nil {
		t.Fatal(err)
	}
	activated, err := store.Activate(context.Background(), memorykit.VersionedCommand{Scope: candidate.Scope, ID: candidate.ID, ExpectedVersion: candidate.Version, Actor: "reviewer-1", Reason: "approved", Now: candidate.UpdatedAt.Add(time.Minute)})
	if err != nil || activated.Status != memorykit.StatusActive {
		t.Fatalf("activated = %#v, %v", activated, err)
	}
	old, err := store.Get(context.Background(), active.Scope, active.ID)
	if err != nil || old.Status != memorykit.StatusInactive {
		t.Fatalf("old = %#v, %v", old, err)
	}
}

func assertSequentialActivationWithEqualTime(t *testing.T, store memorykit.LifecycleStore) {
	active, err := store.Create(context.Background(), baseCreate(
		"43333333-3333-4333-8333-333333333333", "project-1", memorykit.StatusActive))
	if err != nil {
		t.Fatal(err)
	}
	candidateRequest := baseCreate(
		"44444444-4444-4444-8444-444444444444", "project-1", memorykit.StatusCandidate)
	candidateRequest.Content = "Approve a sequential replacement"
	candidate, err := store.Create(context.Background(), candidateRequest)
	if err != nil {
		t.Fatal(err)
	}
	activated, err := store.Activate(context.Background(), memorykit.VersionedCommand{
		Scope: candidate.Scope, ID: candidate.ID, ExpectedVersion: candidate.Version,
		Actor: "reviewer-1", Reason: "sequential replacement", Now: active.UpdatedAt,
	})
	if err != nil || activated.Status != memorykit.StatusActive {
		t.Fatalf("Activate with equal timestamp = %#v, %v", activated, err)
	}
	previous, err := store.Get(context.Background(), active.Scope, active.ID)
	if err != nil || previous.Status != memorykit.StatusInactive {
		t.Fatalf("previous active after equal-time activation = %#v, %v", previous, err)
	}
}

func assertWrongStartingStatusConflicts(t *testing.T, store memorykit.LifecycleStore) {
	tests := []struct {
		name   string
		id     string
		status memorykit.Status
		run    func(memorykit.LifecycleStore, memorykit.VersionedCommand) error
	}{
		{name: "activate active", id: "e8111111-1111-4111-8111-111111111111", status: memorykit.StatusActive,
			run: func(store memorykit.LifecycleStore, command memorykit.VersionedCommand) error {
				_, err := store.Activate(context.Background(), command)
				return err
			}},
		{name: "dismiss active", id: "e8222222-2222-4222-8222-222222222222", status: memorykit.StatusActive,
			run: func(store memorykit.LifecycleStore, command memorykit.VersionedCommand) error {
				_, err := store.Dismiss(context.Background(), command)
				return err
			}},
		{name: "forget candidate", id: "e8333333-3333-4333-8333-333333333333", status: memorykit.StatusCandidate,
			run: func(store memorykit.LifecycleStore, command memorykit.VersionedCommand) error {
				_, err := store.Forget(context.Background(), command)
				return err
			}},
	}
	for index, test := range tests {
		request := baseCreate(test.id, "project-1", test.status)
		request.Key = "wrong.status." + string(rune('a'+index))
		created, err := store.Create(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		beforeRevisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{
			Scope: created.Scope, MemoryID: created.ID, Limit: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		command := memorykit.VersionedCommand{
			Scope: created.Scope, ID: created.ID, ExpectedVersion: created.Version,
			Actor: "reviewer", Reason: "wrong status", Now: created.UpdatedAt.Add(time.Minute),
		}
		if err := test.run(store, command); !errors.Is(err, memorykit.ErrConflict) {
			t.Fatalf("%s error = %v, want ErrConflict", test.name, err)
		}
		after, err := store.Get(context.Background(), created.Scope, created.ID)
		if err != nil || !reflect.DeepEqual(after, created) {
			t.Fatalf("%s changed memory = %#v/%#v, %v", test.name, created, after, err)
		}
		afterRevisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{
			Scope: created.Scope, MemoryID: created.ID, Limit: 10,
		})
		if err != nil || !reflect.DeepEqual(afterRevisions, beforeRevisions) {
			t.Fatalf("%s changed revisions = %#v/%#v, %v", test.name, beforeRevisions, afterRevisions, err)
		}
	}
}

func assertStaleVersionConflicts(t *testing.T, store memorykit.LifecycleStore) {
	active, err := store.Create(context.Background(), baseCreate("51111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusActive))
	if err != nil {
		t.Fatal(err)
	}
	candidateRequest := baseCreate("52222222-2222-2222-2222-222222222222", "project-1", memorykit.StatusCandidate)
	candidateRequest.Content = "Use a stale activation command"
	candidate, err := store.Create(context.Background(), candidateRequest)
	if err != nil {
		t.Fatal(err)
	}
	activeBefore, err := store.Get(context.Background(), active.Scope, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidateBefore, err := store.Get(context.Background(), candidate.Scope, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	activeRevisionsBefore, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: active.Scope, MemoryID: active.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	candidateRevisionsBefore, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: candidate.Scope, MemoryID: candidate.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}

	command := memorykit.VersionedCommand{Scope: candidate.Scope, ID: candidate.ID, ExpectedVersion: candidate.Version + 1, Actor: "reviewer-1", Reason: "stale", Now: candidate.UpdatedAt.Add(time.Minute)}
	if _, err := store.Activate(context.Background(), command); !errors.Is(err, memorykit.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
	activeAfter, err := store.Get(context.Background(), active.Scope, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidateAfter, err := store.Get(context.Background(), candidate.Scope, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(activeAfter, activeBefore) || !reflect.DeepEqual(candidateAfter, candidateBefore) {
		t.Fatalf("memories changed after conflict: active = %#v/%#v, candidate = %#v/%#v", activeBefore, activeAfter, candidateBefore, candidateAfter)
	}
	activeRevisionsAfter, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: active.Scope, MemoryID: active.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	candidateRevisionsAfter, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: candidate.Scope, MemoryID: candidate.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(activeRevisionsAfter, activeRevisionsBefore) || !reflect.DeepEqual(candidateRevisionsAfter, candidateRevisionsBefore) {
		t.Fatalf("revisions changed after conflict: active = %#v/%#v, candidate = %#v/%#v", activeRevisionsBefore, activeRevisionsAfter, candidateRevisionsBefore, candidateRevisionsAfter)
	}
	activeList, err := store.List(context.Background(), memorykit.ListQuery{Scope: active.Scope, Status: memorykit.StatusActive, Limit: 10})
	if err != nil || len(activeList) != 1 || !reflect.DeepEqual(activeList[0], activeBefore) {
		t.Fatalf("active list after conflict = %#v, %v", activeList, err)
	}
}

func assertCorrectionHistory(t *testing.T, store memorykit.LifecycleStore) {
	created, err := store.Create(context.Background(), baseCreate("61111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusActive))
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := store.Correct(context.Background(), memorykit.CorrectRequest{
		Command: memorykit.VersionedCommand{Scope: created.Scope, ID: created.ID, ExpectedVersion: created.Version, Actor: "user-1", Reason: "correct command", Now: created.UpdatedAt.Add(time.Minute)},
		Content: "Run go test -race ./...", ValidFrom: created.ValidFrom, Importance: created.Importance, Confidence: 1,
	})
	if err != nil || corrected.Version != created.Version+1 {
		t.Fatalf("corrected = %#v, %v", corrected, err)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: created.Scope, MemoryID: created.ID, Limit: 10})
	if err != nil || len(revisions) != 2 || revisions[0].Action != memorykit.RevisionCorrect || revisions[1].Action != memorykit.RevisionCreate {
		t.Fatalf("revisions = %#v, %v", revisions, err)
	}
	before, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: created.Scope, MemoryID: created.ID, BeforeVersion: corrected.Version, Limit: 10})
	if err != nil || len(before) != 1 || before[0].Version != created.Version {
		t.Fatalf("cursor revisions = %#v, %v", before, err)
	}
}

func assertDismissedCandidateInactive(t *testing.T, store memorykit.LifecycleStore) {
	candidate, err := store.Create(context.Background(), baseCreate("71111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusCandidate))
	if err != nil {
		t.Fatal(err)
	}
	dismissed, err := store.Dismiss(context.Background(), memorykit.VersionedCommand{Scope: candidate.Scope, ID: candidate.ID, ExpectedVersion: candidate.Version, Actor: "reviewer-1", Reason: "unsupported", Now: candidate.UpdatedAt.Add(time.Minute)})
	if err != nil || dismissed.Status != memorykit.StatusInactive {
		t.Fatalf("dismissed = %#v, %v", dismissed, err)
	}
}

func assertForgetSemantics(t *testing.T, store memorykit.LifecycleStore) {
	created, err := store.Create(context.Background(), baseCreate("81111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusActive))
	if err != nil {
		t.Fatal(err)
	}
	forgotten, err := store.Forget(context.Background(), memorykit.VersionedCommand{Scope: created.Scope, ID: created.ID, ExpectedVersion: created.Version, Actor: "user-1", Reason: "forget", Now: created.UpdatedAt.Add(time.Minute)})
	if err != nil || forgotten.Status != memorykit.StatusInactive {
		t.Fatalf("forgotten = %#v, %v", forgotten, err)
	}
	active, err := store.List(context.Background(), memorykit.ListQuery{Scope: created.Scope, Status: memorykit.StatusActive, Limit: 10})
	if err != nil || len(active) != 0 {
		t.Fatalf("active after forget = %#v, %v", active, err)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: created.Scope, MemoryID: created.ID, Limit: 10})
	if err != nil || len(revisions) != 2 || revisions[0].Action != memorykit.RevisionForget || revisions[0].Snapshot.Content == "" {
		t.Fatalf("revisions = %#v, %v", revisions, err)
	}
}

func assertEraseSemantics(t *testing.T, store memorykit.LifecycleStore) {
	created, err := store.Create(context.Background(), baseCreate("91111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusActive))
	if err != nil {
		t.Fatal(err)
	}
	sources, err := store.Sources(context.Background(), created.Scope, created.ID)
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources before erase = %#v, %v", sources, err)
	}
	if err := store.Erase(context.Background(), memorykit.VersionedCommand{Scope: created.Scope, ID: created.ID, ExpectedVersion: created.Version, Actor: "privacy-admin", Reason: "erasure", Now: created.UpdatedAt.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	erased, err := store.Get(context.Background(), created.Scope, created.ID)
	if err != nil || erased.Content != "" || erased.Status != memorykit.StatusInactive {
		t.Fatalf("erased = %#v, %v", erased, err)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: created.Scope, MemoryID: created.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range revisions {
		if revision.Snapshot.Content != "" || !revision.ContentErased {
			t.Fatalf("unscrubbed revision = %#v", revision)
		}
	}
	sources, err = store.Sources(context.Background(), created.Scope, created.ID)
	if err != nil || sources == nil || len(sources) != 0 {
		t.Fatalf("sources after erase = %#v, %v", sources, err)
	}
}

func assertDefensiveCopies(t *testing.T, store memorykit.LifecycleStore) {
	request := baseCreate("a1111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusActive)
	created, err := store.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Sources[0].Ref = "mutated-input"
	created.Content = "mutated"
	loaded, err := store.Get(context.Background(), created.Scope, created.ID)
	if err != nil || loaded.Content == "mutated" {
		t.Fatalf("loaded = %#v, %v", loaded, err)
	}
	sources, err := store.Sources(context.Background(), loaded.Scope, loaded.ID)
	if err != nil || sources[0].Ref == "mutated-input" {
		t.Fatalf("sources = %#v, %v", sources, err)
	}
	sources[0].Ref = "mutated-output"
	again, err := store.Sources(context.Background(), loaded.Scope, loaded.ID)
	if err != nil || again[0].Ref == "mutated-output" {
		t.Fatalf("sources after output mutation = %#v, %v", again, err)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: loaded.Scope, MemoryID: loaded.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	revisions[0].Snapshot.Content = "mutated-revision"
	againRevisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: loaded.Scope, MemoryID: loaded.ID, Limit: 10})
	if err != nil || againRevisions[0].Snapshot.Content == "mutated-revision" {
		t.Fatalf("revisions after output mutation = %#v, %v", againRevisions, err)
	}
}

func assertCancellation(t *testing.T, store memorykit.LifecycleStore) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scope := memorykit.Scope{TenantID: "tenant-conformance", SubjectType: memorykit.SubjectProject, SubjectID: "project-1"}
	checks := []struct {
		name string
		run  func() error
	}{
		{"Create", func() error { _, err := store.Create(ctx, memorykit.CreateRequest{}); return err }},
		{"Get", func() error { _, err := store.Get(ctx, memorykit.Scope{}, ""); return err }},
		{"Sources", func() error { _, err := store.Sources(ctx, memorykit.Scope{}, ""); return err }},
		{"List", func() error { _, err := store.List(ctx, memorykit.ListQuery{Scope: scope, Limit: 10}); return err }},
		{"Activate", func() error { _, err := store.Activate(ctx, memorykit.VersionedCommand{}); return err }},
		{"Correct", func() error { _, err := store.Correct(ctx, memorykit.CorrectRequest{}); return err }},
		{"Dismiss", func() error { _, err := store.Dismiss(ctx, memorykit.VersionedCommand{}); return err }},
		{"Forget", func() error { _, err := store.Forget(ctx, memorykit.VersionedCommand{}); return err }},
		{"Erase", func() error { return store.Erase(ctx, memorykit.VersionedCommand{}) }},
		{"Revisions", func() error { _, err := store.Revisions(ctx, memorykit.RevisionQuery{}); return err }},
	}
	for _, check := range checks {
		if err := check.run(); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s error = %v", check.name, err)
		}
	}
}

func assertIdempotentReplay(t *testing.T, store memorykit.LifecycleStore) {
	request := baseCreate("b1111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusCandidate)
	request.IdempotencyKey = "create:decision:1"
	created, err := store.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := store.Correct(context.Background(), memorykit.CorrectRequest{
		Command: memorykit.VersionedCommand{Scope: created.Scope, ID: created.ID, ExpectedVersion: created.Version, Actor: "user-2", Reason: "correction", Now: created.UpdatedAt.Add(time.Minute)},
		Content: "Run go test -race ./...", ValidFrom: created.ValidFrom, Importance: created.Importance, Confidence: created.Confidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	request.Now = request.Now.Add(24 * time.Hour)
	replayed, err := store.Create(context.Background(), request)
	if err != nil || !reflect.DeepEqual(replayed, corrected) {
		t.Fatalf("replayed/current = %#v/%#v, %v", replayed, corrected, err)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: created.Scope, MemoryID: created.ID, Limit: 10})
	if err != nil || len(revisions) != 2 {
		t.Fatalf("revisions after replay = %#v, %v", revisions, err)
	}
}

func assertSourceOrderParticipatesInIdempotency(t *testing.T, store memorykit.LifecycleStore) {
	request := baseCreate("b2222222-2222-4222-8222-222222222222", "project-1", memorykit.StatusCandidate)
	request.IdempotencyKey = "create:decision:source-order"
	request.Sources = []memorykit.Source{
		{Kind: "artifact", Ref: "artifact:first", EvidenceHash: "sha256:first"},
		{Kind: "message", Ref: "message:second", EvidenceHash: "sha256:second"},
	}
	if _, err := store.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	reordered := request
	reordered.Sources = []memorykit.Source{request.Sources[1], request.Sources[0]}
	if _, err := store.Create(context.Background(), reordered); !errors.Is(err, memorykit.ErrConflict) {
		t.Fatalf("reordered source replay error = %v, want ErrConflict", err)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{
		Scope: request.Scope, MemoryID: request.ID, Limit: 10,
	})
	if err != nil || len(revisions) != 1 {
		t.Fatalf("revisions after reordered replay = %#v, %v", revisions, err)
	}
}

func assertIdempotencyConflicts(t *testing.T, store memorykit.LifecycleStore) {
	original := baseCreate("c1111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusCandidate)
	original.IdempotencyKey = "create:decision:2"
	if _, err := store.Create(context.Background(), original); err != nil {
		t.Fatal(err)
	}

	cases := []memorykit.CreateRequest{
		func() memorykit.CreateRequest { changed := original; changed.Content = "different"; return changed }(),
		func() memorykit.CreateRequest {
			changed := original
			changed.ID = "c2222222-2222-2222-2222-222222222222"
			return changed
		}(),
		func() memorykit.CreateRequest {
			changed := original
			changed.IdempotencyKey = "different-key"
			return changed
		}(),
		func() memorykit.CreateRequest {
			changed := original
			changed.Scope.SubjectID = "project-2"
			changed.IdempotencyKey = ""
			return changed
		}(),
	}
	for i, request := range cases {
		if _, err := store.Create(context.Background(), request); !errors.Is(err, memorykit.ErrConflict) {
			t.Fatalf("conflict case %d error = %v", i, err)
		}
	}

	otherScope := original
	otherScope.ID = "c3333333-3333-3333-3333-333333333333"
	otherScope.Scope.SubjectID = "project-2"
	if _, err := store.Create(context.Background(), otherScope); err != nil {
		t.Fatalf("same idempotency key in another scope: %v", err)
	}
	loaded, err := store.Get(context.Background(), original.Scope, original.ID)
	if err != nil || loaded.Content != original.Content {
		t.Fatalf("original after conflicts = %#v, %v", loaded, err)
	}
}

func assertEraseInvalidatesReplay(t *testing.T, store memorykit.LifecycleStore) {
	request := baseCreate("d1111111-1111-1111-1111-111111111111", "project-1", memorykit.StatusActive)
	request.IdempotencyKey = "create:decision:erased"
	created, err := store.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Erase(context.Background(), memorykit.VersionedCommand{
		Scope: created.Scope, ID: created.ID, ExpectedVersion: created.Version,
		Actor: "privacy-admin", Reason: "erasure", Now: created.UpdatedAt.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), request); !errors.Is(err, memorykit.ErrConflict) {
		t.Fatalf("replay after erase error = %v", err)
	}
	erased, err := store.Get(context.Background(), created.Scope, created.ID)
	if err != nil || erased.Content != "" || erased.Status != memorykit.StatusInactive {
		t.Fatalf("erased after replay = %#v, %v", erased, err)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{Scope: created.Scope, MemoryID: created.ID, Limit: 10})
	if err != nil || len(revisions) != 2 {
		t.Fatalf("revisions after erased replay = %#v, %v", revisions, err)
	}
}

func assertNonCanonicalIdentityRejected(t *testing.T, store memorykit.LifecycleStore) {
	request := baseCreate("f1111111-1111-4111-8111-111111111111", "project-1", memorykit.StatusCandidate)
	created, err := store.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonical := strings.ToUpper(created.ID)
	if _, err := store.Get(context.Background(), created.Scope, nonCanonical); !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("Get non-canonical ID error = %v, want ErrInvalidMemory", err)
	}
	if _, err := store.Sources(context.Background(), created.Scope, nonCanonical); !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("Sources non-canonical ID error = %v, want ErrInvalidMemory", err)
	}
}
