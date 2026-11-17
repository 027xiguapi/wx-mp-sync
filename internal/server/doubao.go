package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	doubaoarchive "wx-mp-sync/internal/doubao"
	scraper "wx-mp-sync/pkg/scraper/doubao"
)

const (
	// maxDoubaoURLs 是单次豆包批量下载允许的链接数上限。
	maxDoubaoURLs = 50
	// maxDoubaoBody 是豆包批量请求体的上限。
	maxDoubaoBody = 1 << 20
	// doubaoJobTTL 是豆包任务在内存中保留的时长。
	doubaoJobTTL = 2 * time.Hour
	// defaultDoubaoDelay 是两篇之间的间隔,避免请求过快。
	defaultDoubaoDelay = 300 * time.Millisecond
)

// doubaoArchiver 是豆包归档器接口,便于测试替换。
type doubaoArchiver interface {
	Save(ctx context.Context, rawURL string) (*doubaoarchive.Result, error)
}

// DoubaoItem 是豆包批量任务里的一条链接。
type DoubaoItem struct {
	URL    string `json:"url"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state"`
	Dir    string `json:"dir,omitempty"`
	Error  string `json:"error,omitempty"`
	Images int    `json:"images,omitempty"`
}

// DoubaoSnapshot 是豆包批量任务的只读快照。
type DoubaoSnapshot struct {
	ID         string       `json:"id"`
	CreatedAt  time.Time    `json:"created_at"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	Total      int          `json:"total"`
	Done       int          `json:"done"`
	OK         int          `json:"ok"`
	Skipped    int          `json:"skipped"`
	Failed     int          `json:"failed"`
	Running    bool         `json:"running"`
	Finished   bool         `json:"finished"`
	Canceled   bool         `json:"canceled"`
	Items      []DoubaoItem `json:"items"`
}

// DoubaoJob 是一次豆包批量下载任务。
type DoubaoJob struct {
	mu       sync.Mutex
	snap     DoubaoSnapshot
	cancelCh chan struct{}
	canceled bool
}

// doubaoManager 持有豆包任务并清理过期项。
type doubaoManager struct {
	mu   sync.Mutex
	jobs map[string]*DoubaoJob
}

func newDoubaoManager() *doubaoManager {
	return &doubaoManager{jobs: map[string]*DoubaoJob{}}
}

func newDoubaoID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("db%d", time.Now().UnixNano())
	}
	return "db" + hex.EncodeToString(buf)
}

// ID 返回任务 id。
func (j *DoubaoJob) ID() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snap.ID
}

// snapshot 返回任务只读副本。
func (j *DoubaoJob) snapshot() DoubaoSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.snap
	out.Items = make([]DoubaoItem, len(j.snap.Items))
	copy(out.Items, j.snap.Items)
	return out
}

func (j *DoubaoJob) canceledFlag() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.canceled
}

func (j *DoubaoJob) cancel() {
	j.mu.Lock()
	if !j.canceled {
		j.canceled = true
		close(j.cancelCh)
	}
	j.mu.Unlock()
}

func (j *DoubaoJob) setRunning(i int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if i < len(j.snap.Items) {
		j.snap.Items[i].State = batchStateRunning
	}
	j.snap.Running = true
}

func (j *DoubaoJob) finishItem(i int, state, dir, title, errMsg string, images int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if i >= len(j.snap.Items) {
		return
	}
	item := &j.snap.Items[i]
	item.State = state
	item.Dir = dir
	item.Error = errMsg
	item.Images = images
	if title != "" {
		item.Title = title
	}
	j.snap.Done++
	switch state {
	case batchStateOK:
		j.snap.OK++
	case batchStateSkipped:
		j.snap.Skipped++
	case batchStateFailed:
		j.snap.Failed++
	}
}

func (j *DoubaoJob) finish(canceled bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	j.snap.Running = false
	j.snap.Finished = true
	j.snap.Canceled = canceled
	j.snap.FinishedAt = &now
}

func (m *doubaoManager) add(j *DoubaoJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, old := range m.jobs {
		snap := old.snapshot()
		if snap.Finished && snap.FinishedAt != nil && time.Since(*snap.FinishedAt) > doubaoJobTTL {
			delete(m.jobs, id)
		}
	}
	m.jobs[j.ID()] = j
}

func (m *doubaoManager) get(id string) (*DoubaoJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	return j, ok
}

// doubaos 返回豆包任务管理器(按需初始化)。
func (s *Server) doubaos() *doubaoManager {
	s.doubaoMu.Lock()
	defer s.doubaoMu.Unlock()
	if s.doubaoJobs == nil {
		s.doubaoJobs = newDoubaoManager()
	}
	return s.doubaoJobs
}

// SetDoubaoArchiver 注入豆包归档器(由 app 编排层调用)。
func (s *Server) SetDoubaoArchiver(a doubaoArchiver) {
	s.doubaoArchiver = a
}

// handleDoubaoBatchCreate 接收一组豆包分享链接,创建后台下载任务。
func (s *Server) handleDoubaoBatchCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDoubaoBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取请求失败"})
		return
	}
	var req struct {
		URLs []batchURL `json:"urls"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式错误"})
		return
	}

	items, dropped := normalizeDoubaoURLs(req.URLs)
	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有可下载的豆包分享链接"})
		return
	}
	if len(items) > maxDoubaoURLs {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("一次最多 %d 个链接,当前 %d 个", maxDoubaoURLs, len(items)),
		})
		return
	}
	if s.doubaoArchiver == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "豆包归档器未初始化"})
		return
	}

	job := &DoubaoJob{
		snap: DoubaoSnapshot{
			ID:        newDoubaoID(),
			CreatedAt: time.Now(),
			Total:     len(items),
			Items:     items,
		},
		cancelCh: make(chan struct{}),
	}
	s.doubaos().add(job)
	go s.runDoubaoJob(job)

	s.logger.Info().Str("job", job.ID()).Int("total", len(items)).Int("dropped", dropped).Msg("创建豆包批量下载任务")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "job_id": job.ID(), "total": len(items), "dropped": dropped,
	})
}

// normalizeDoubaoURLs 过滤非法/重复链接,按分享 id 去重。
func normalizeDoubaoURLs(in []batchURL) ([]DoubaoItem, int) {
	items := make([]DoubaoItem, 0, len(in))
	seen := map[string]bool{}
	dropped := 0
	for _, raw := range in {
		rawURL := strings.TrimSpace(raw.URL)
		shareID, err := scraper.ParseShareID(rawURL)
		if err != nil {
			if rawURL != "" {
				dropped++
			}
			continue
		}
		if seen[shareID] {
			dropped++
			continue
		}
		seen[shareID] = true
		items = append(items, DoubaoItem{
			URL:   scraper.CanonicalURL(shareID),
			Title: strings.TrimSpace(raw.Title),
			State: batchStatePending,
		})
	}
	return items, dropped
}

// runDoubaoJob 顺序归档任务里的分享。
func (s *Server) runDoubaoJob(job *DoubaoJob) {
	total := len(job.snapshot().Items)
	for i := 0; i < total; i++ {
		if job.canceledFlag() {
			break
		}
		job.mu.Lock()
		rawURL := job.snap.Items[i].URL
		job.mu.Unlock()

		job.setRunning(i)
		result, err := s.doubaoArchiver.Save(context.Background(), rawURL)
		switch {
		case err != nil:
			s.logger.Error().Str("job", job.ID()).Str("url", rawURL).Err(err).Msg("豆包下载失败")
			job.finishItem(i, batchStateFailed, "", "", err.Error(), 0)
		case result.Duplicate:
			s.logger.Info().Str("job", job.ID()).Str("url", rawURL).Str("dir", result.Dir).Msg("豆包下载跳过(已存在)")
			job.finishItem(i, batchStateSkipped, result.Dir, result.Title, "", result.Images)
		default:
			s.logger.Info().Str("job", job.ID()).Str("title", result.Title).Str("dir", result.Dir).Msg("豆包下载成功")
			job.finishItem(i, batchStateOK, result.Dir, result.Title, "", result.Images)
		}

		if i < total-1 && !job.canceledFlag() {
			timer := time.NewTimer(defaultDoubaoDelay)
			select {
			case <-timer.C:
			case <-job.cancelCh:
				timer.Stop()
			}
		}
	}

	canceled := job.canceledFlag()
	if canceled {
		job.mu.Lock()
		for i := range job.snap.Items {
			if job.snap.Items[i].State == batchStatePending {
				job.snap.Items[i].State = batchStateCanceled
			}
		}
		job.mu.Unlock()
	}
	job.finish(canceled)
}

// findDoubaoJob 从查询参数 id 取任务。
func (s *Server) findDoubaoJob(r *http.Request) (*DoubaoJob, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		return nil, false
	}
	return s.doubaos().get(id)
}

// handleDoubaoStatus 返回豆包任务进度。
func (s *Server) handleDoubaoStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.findDoubaoJob(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "任务不存在或已过期"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job.snapshot()})
}

// handleDoubaoCancel 请求取消任务。
func (s *Server) handleDoubaoCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	job, ok := s.findDoubaoJob(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "任务不存在或已过期"})
		return
	}
	job.cancel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDoubaoZip 把任务里成功的分享打包成 ZIP 返回。
func (s *Server) handleDoubaoZip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	job, ok := s.findDoubaoJob(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "任务不存在或已过期"})
		return
	}
	folders, manifest, count := buildDoubaoFolders(job.snapshot())
	if count == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有可打包的豆包分享"})
		return
	}

	tmp, err := os.CreateTemp("", "doubao-batch-*.zip")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "创建临时文件失败: " + err.Error()})
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	defer tmp.Close()

	if err := writeZipArchive(tmp, folders, []zipFile{{Name: "_manifest.json", Data: manifest}}); err != nil {
		s.logger.Error().Err(err).Msg("豆包批量打包失败")
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "打包失败: " + err.Error()})
		return
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "打包失败: " + err.Error()})
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "打包失败: " + err.Error()})
		return
	}

	snap := job.snapshot()
	name := fmt.Sprintf("doubao-batch-%s.zip", snap.CreatedAt.Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(name))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition, X-Batch-Ok, X-Batch-Total")
	w.Header().Set("X-Batch-Ok", fmt.Sprintf("%d", count))
	w.Header().Set("X-Batch-Total", fmt.Sprintf("%d", snap.Total))
	if _, err := io.Copy(w, tmp); err != nil {
		s.logger.Warn().Err(err).Msg("输出豆包 ZIP 失败")
	}
}

// buildDoubaoFolders 由任务快照生成 ZIP 目录列表与清单。
func buildDoubaoFolders(snap DoubaoSnapshot) ([]zipFolder, []byte, int) {
	var folders []zipFolder
	used := map[string]bool{}
	type manifestItem struct {
		URL    string `json:"url"`
		Title  string `json:"title,omitempty"`
		Folder string `json:"folder,omitempty"`
		State  string `json:"state"`
		Error  string `json:"error,omitempty"`
		Images int    `json:"images,omitempty"`
	}
	items := make([]manifestItem, 0, len(snap.Items))

	for _, item := range snap.Items {
		if (item.State != batchStateOK && item.State != batchStateSkipped) || item.Dir == "" {
			items = append(items, manifestItem{
				URL: item.URL, Title: item.Title, State: item.State, Error: item.Error, Images: item.Images,
			})
			continue
		}
		if st, err := os.Stat(item.Dir); err != nil || !st.IsDir() {
			items = append(items, manifestItem{
				URL: item.URL, Title: item.Title, State: item.State, Error: "归档目录不存在", Images: item.Images,
			})
			continue
		}
		title := readMetaTitle(item.Dir)
		if strings.TrimSpace(title) == "" {
			title = strings.TrimSpace(item.Title)
		}
		if strings.TrimSpace(title) == "" {
			title = filepath.Base(item.Dir)
		}
		folder := uniqueFolderName(sanitizeFileName(title), used)
		folders = append(folders, zipFolder{Prefix: folder, Dir: item.Dir})
		items = append(items, manifestItem{
			URL: item.URL, Title: title, Folder: folder, State: item.State, Images: item.Images,
		})
	}

	manifest, err := json.MarshalIndent(map[string]any{
		"job_id":     snap.ID,
		"created_at": snap.CreatedAt.Format(time.RFC3339),
		"total":      snap.Total,
		"ok":         snap.OK,
		"skipped":    snap.Skipped,
		"failed":     snap.Failed,
		"items":      items,
	}, "", "  ")
	if err != nil {
		manifest = []byte("{}")
	}
	return folders, manifest, len(folders)
}

// handleDoubaoOne 归档单个豆包分享并直接返回 ZIP(供注入按钮调用)。
func (s *Server) handleDoubaoOne(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	rawURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取请求失败"})
			return
		}
		var req struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(body, &req); err == nil && strings.TrimSpace(req.URL) != "" {
			rawURL = strings.TrimSpace(req.URL)
		}
	}
	if rawURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 url"})
		return
	}
	if _, err := scraper.ParseShareID(rawURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if s.doubaoArchiver == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "豆包归档器未初始化"})
		return
	}

	result, err := s.doubaoArchiver.Save(r.Context(), rawURL)
	if err != nil {
		s.logger.Error().Str("url", rawURL).Err(err).Msg("豆包单篇下载失败")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.logger.Info().Str("url", rawURL).Str("dir", result.Dir).Bool("duplicate", result.Duplicate).Msg("豆包单篇打包下载")
	s.writeArticleZip(w, result.Dir, result.Title, result.Duplicate)
}
