package naicha

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Record 记录某个归档目录已经发布到论坛的帖子。
type Record struct {
	ID       int    `json:"id,omitempty"`
	PostID   string `json:"post_id,omitempty"` // 新版接口的帖子 ID(UUID)
	URL      string `json:"url"`
	Title    string `json:"title"`
	SyncedAt string `json:"synced_at"`
}

// stateStore 是进程内的去重状态,持久化到输出目录下的 JSON 文件。
type stateStore struct {
	mu     sync.Mutex
	path   string
	loaded bool
	posts  map[string]Record
}

type stateFile struct {
	Posts map[string]Record `json:"posts"`
}

func newStateStore(path string) *stateStore {
	return &stateStore{path: path, posts: make(map[string]Record)}
}

func (s *stateStore) ensureLoaded() {
	if s.loaded {
		return
	}
	s.loaded = true
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var file stateFile
	if err := json.Unmarshal(data, &file); err != nil {
		return
	}
	if file.Posts != nil {
		s.posts = file.Posts
	}
}

// Get 查询目录对应的发布记录。
func (s *stateStore) Get(dir string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoaded()
	rec, ok := s.posts[dir]
	return rec, ok
}

// Put 写入发布记录并持久化。
func (s *stateStore) Put(dir string, rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoaded()
	s.posts[dir] = rec
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(stateFile{Posts: s.posts}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".naicha_state_*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
