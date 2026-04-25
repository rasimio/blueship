package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// PromptStore returns named prompt strings to handlers that need them.
// Implementations: file-backed (FilePromptStore) is the canonical option;
// agents may also inject an in-memory map via NewMapPromptStore for tests.
type PromptStore interface {
	Get(ctx context.Context, key string) (string, error)
	GetAll(ctx context.Context) (map[string]string, error)
}

// FilePromptStore reads prompts from a directory of `<key>.md` files.
//
// Lookups are case-sensitive on key. Missing files surface as an error
// from Get; callers that treat absence as optional should ignore the
// error and use a default. Reads are cached after first access.
type FilePromptStore struct {
	dir   string
	mu    sync.RWMutex
	cache map[string]string
}

// NewFilePromptStore returns a store rooted at dir. Empty dir returns an
// implementation whose Get always errors — useful when the agent has no
// prompts directory at all.
func NewFilePromptStore(dir string) *FilePromptStore {
	return &FilePromptStore{dir: dir, cache: map[string]string{}}
}

// Dir returns the configured directory (for diagnostics).
func (s *FilePromptStore) Dir() string { return s.dir }

func (s *FilePromptStore) Get(ctx context.Context, key string) (string, error) {
	s.mu.RLock()
	if v, ok := s.cache[key]; ok {
		s.mu.RUnlock()
		return v, nil
	}
	s.mu.RUnlock()
	if s.dir == "" {
		return "", fmt.Errorf("prompt %q: no prompts directory configured", key)
	}
	path := filepath.Join(s.dir, key+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("prompt %q: %w", key, err)
	}
	v := string(data)
	s.mu.Lock()
	s.cache[key] = v
	s.mu.Unlock()
	return v, nil
}

func (s *FilePromptStore) GetAll(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if s.dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list prompts dir %q: %w", s.dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		key := strings.TrimSuffix(e.Name(), ".md")
		v, err := s.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	return out, nil
}

// MapPromptStore is a small in-memory PromptStore for tests and embedded
// builds where prompts are bundled at compile time.
type MapPromptStore struct {
	prompts map[string]string
}

// NewMapPromptStore wraps a key→content map. The map is not copied; the
// caller must not mutate it after construction.
func NewMapPromptStore(prompts map[string]string) *MapPromptStore {
	return &MapPromptStore{prompts: prompts}
}

func (s *MapPromptStore) Get(_ context.Context, key string) (string, error) {
	v, ok := s.prompts[key]
	if !ok {
		return "", fmt.Errorf("prompt %q: not found", key)
	}
	return v, nil
}

func (s *MapPromptStore) GetAll(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.prompts))
	for k, v := range s.prompts {
		out[k] = v
	}
	return out, nil
}
