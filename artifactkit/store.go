package artifactkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var ErrArtifactNotFound = errors.New("artifact not found")

type Artifact struct {
	Ref         string
	Content     []byte
	ContentType string
	Metadata    map[string]any
}

type Store interface {
	Put(context.Context, Artifact) error
	Get(context.Context, string) (Artifact, error)
}

type MemoryStore struct {
	mu        sync.RWMutex
	artifacts map[string]Artifact
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{artifacts: map[string]Artifact{}}
}

func (s *MemoryStore) Put(ctx context.Context, artifact Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(artifact.Ref) == "" {
		return fmt.Errorf("artifact ref is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[artifact.Ref] = cloneArtifact(artifact)
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, ref string) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	artifact, ok := s.artifacts[ref]
	if !ok {
		return Artifact{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, ref)
	}
	return cloneArtifact(artifact), nil
}

func cloneArtifact(artifact Artifact) Artifact {
	artifact.Content = append([]byte(nil), artifact.Content...)
	artifact.Metadata = cloneMetadata(artifact.Metadata)
	return artifact
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	copied := make(map[string]any, len(metadata))
	for key, value := range metadata {
		copied[key] = value
	}
	return copied
}
