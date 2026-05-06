package artifactkit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type FileStore struct {
	root string
}

type fileArtifact struct {
	Ref         string         `json:"ref"`
	Content     []byte         `json:"content"`
	ContentType string         `json:"content_type,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at,omitempty"`
}

func NewFileStore(root string) (*FileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("artifact root is required")
	}
	clean := filepath.Clean(root)
	if err := os.MkdirAll(filepath.Join(clean, "objects"), 0o700); err != nil {
		return nil, err
	}
	return &FileStore{root: clean}, nil
}

func (s *FileStore) Put(ctx context.Context, artifact Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(artifact.Ref) == "" {
		return fmt.Errorf("artifact ref is required")
	}
	record := fileArtifact{
		Ref:         artifact.Ref,
		Content:     append([]byte(nil), artifact.Content...),
		ContentType: artifact.ContentType,
		Metadata:    cloneMetadata(artifact.Metadata),
		CreatedAt:   time.Now().UTC(),
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	path := s.path(artifact.Ref)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func (s *FileStore) Get(ctx context.Context, ref string) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	raw, err := os.ReadFile(s.path(ref))
	if os.IsNotExist(err) {
		return Artifact{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, ref)
	}
	if err != nil {
		return Artifact{}, err
	}
	var record fileArtifact
	if err := json.Unmarshal(raw, &record); err != nil {
		return Artifact{}, err
	}
	return cloneArtifact(Artifact{
		Ref:         record.Ref,
		Content:     record.Content,
		ContentType: record.ContentType,
		Metadata:    record.Metadata,
	}), nil
}

func (s *FileStore) path(ref string) string {
	name := base64.RawURLEncoding.EncodeToString([]byte(ref)) + ".json"
	return filepath.Join(s.root, "objects", name)
}
