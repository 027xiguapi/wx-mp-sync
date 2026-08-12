// Package store 提供同步状态的持久化(JSON 文件),用于文章去重。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Record 记录一篇已同步文章的状态。
type Record struct {
	ExternalID string `json:"external_id"` // 文章唯一标识(biz_mid_idx 或 url 派生)
	URL        string `json:"url"`
	Title      string `json:"title"`
	Dir        string `json:"dir"`      // 文章归档目录
	HTML       string `json:"html"`     // HTML 文件名
	Markdown   string `json:"markdown"` // Markdown 文件名
	SyncedAt   int64  `json:"synced_at"`
}

// Store 是一个带原子写能力的键值状态存储。
type Store struct {
	mu    sync.Mutex
	path  string
	items map[string]*Record
}

// New 打开(或创建)状态文件。文件不存在或损坏时回退到空状态。
func New(path string) (*Store, error) {
	s := &Store{
		path:  path,
		items: map[string]*Record{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load 从磁盘加载状态,文件不存在时视为空。
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &s.items); err != nil {
		// 状态文件损坏时重置,避免阻塞同步
		return nil
	}
	return nil
}

// Has 判断指定 key 是否已存在。
func (s *Store) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.items[key]
	return ok
}

// Get 返回指定 key 对应的记录。
func (s *Store) Get(key string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	return r, ok
}

// Put 写入(或更新)一条记录并立即落盘。
func (s *Store) Put(key string, rec *Record) error {
	s.mu.Lock()
	s.items[key] = rec
	s.mu.Unlock()
	return s.Save()
}

// Save 把全部记录原子写回磁盘(临时文件 + rename)。
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态失败: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建状态目录失败: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".wx_mp_sync_state_*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时状态文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("写入临时状态文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("同步临时状态文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("关闭临时状态文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("替换状态文件失败: %w", err)
	}
	return nil
}
