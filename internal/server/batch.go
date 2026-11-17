package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wx-mp-sync/pkg/scraper/wxmp"
)

const (
	// maxBatchURLs 是单次批量下载允许的文章数上限。
	maxBatchURLs = 100
	// maxBatchBody 是批量请求体的上限。
	maxBatchBody = 1 << 20
	// batchJobTTL 是批量任务在内存中保留的时长。
	batchJobTTL = 2 * time.Hour
	// defaultBatchDelay 是批量抓取时两篇之间的间隔,避免触发微信限流。
	defaultBatchDelay = 900 * time.Millisecond
)

// 批量任务中单篇文章的状态。
const (
	batchStatePending  = "pending"
	batchStateRunning  = "running"
	batchStateOK       = "ok"
	batchStateSkipped  = "skipped"
	batchStateFailed   = "failed"
	batchStateCanceled = "canceled"
)

// BatchItem 是批量任务里的一篇文章。
type BatchItem struct {
	URL    string `json:"url"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state"`
	Dir    string `json:"dir,omitempty"`
	Error  string `json:"error,omitempty"`
	Images int    `json:"images,omitempty"`
}

// BatchSnapshot 是批量任务的只读快照,用于 JSON 响应(不含锁)。
type BatchSnapshot struct {
	ID         string      `json:"id"`
	CreatedAt  time.Time   `json:"created_at"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
	Total      int         `json:"total"`
	Done       int         `json:"done"`
	OK         int         `json:"ok"`
	Skipped    int         `json:"skipped"`
	Failed     int         `json:"failed"`
	Running    bool        `json:"running"`
	Finished   bool        `json:"finished"`
	Canceled   bool        `json:"canceled"`
	Items      []BatchItem `json:"items"`
}

// BatchJob 是一次批量下载任务。内部状态由 mu 保护。
type BatchJob struct {
	mu       sync.Mutex
	snap     BatchSnapshot
	cancelCh chan struct{}
	canceled bool
}

// snapshot 返回任务的只读副本。
func (j *BatchJob) snapshot() BatchSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.snap
	out.Items = make([]BatchItem, len(j.snap.Items))
	copy(out.Items, j.snap.Items)
	return out
}

// canceledFlag 报告任务是否已请求取消。
func (j *BatchJob) canceledFlag() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.canceled
}

// cancel 请求取消任务(仅影响尚未开始的条目)。
func (j *BatchJob) cancel() {
	j.mu.Lock()
	if !j.canceled {
		j.canceled = true
		close(j.cancelCh)
	}
	j.mu.Unlock()
}

// setRunning 把第 i 条标记为进行中。
func (j *BatchJob) setRunning(i int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if i < len(j.snap.Items) {
		j.snap.Items[i].State = batchStateRunning
	}
	j.snap.Running = true
}

// finishItem 记录第 i 条的最终结果(拿到真实文章标题时覆盖页面上抓到的标题)。
func (j *BatchJob) finishItem(i int, state, dir, title, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if i >= len(j.snap.Items) {
		return
	}
	item := &j.snap.Items[i]
	item.State = state
	item.Dir = dir
	item.Error = errMsg
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

// setItemImages 记录第 i 条归档到的图片数。
func (j *BatchJob) setItemImages(i, images int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if i >= 0 && i < len(j.snap.Items) {
		j.snap.Items[i].Images = images
	}
}

// finish 标记任务结束。
func (j *BatchJob) finish(canceled bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	j.snap.Running = false
	j.snap.Finished = true
	j.snap.Canceled = canceled
	j.snap.FinishedAt = &now
}

// batchManager 持有所有批量任务,并把过期任务清理掉。
type batchManager struct {
	mu   sync.Mutex
	jobs map[string]*BatchJob
}

// newBatchManager 创建任务管理器。
func newBatchManager() *batchManager {
	return &batchManager{jobs: map[string]*BatchJob{}}
}

// add 登记一个新任务,同时清理过期任务。
func (m *batchManager) add(j *BatchJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, old := range m.jobs {
		snap := old.snapshot()
		if snap.Finished && snap.FinishedAt != nil && time.Since(*snap.FinishedAt) > batchJobTTL {
			delete(m.jobs, id)
		}
	}
	m.jobs[j.ID()] = j
}

// get 按 id 取任务。
func (m *batchManager) get(id string) (*BatchJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	return j, ok
}

// ID 返回任务 id。
func (j *BatchJob) ID() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snap.ID
}

// newBatchJob 由链接列表构造任务。
func newBatchJob(items []BatchItem) *BatchJob {
	return &BatchJob{
		snap: BatchSnapshot{
			ID:        newBatchID(),
			CreatedAt: time.Now(),
			Total:     len(items),
			Items:     items,
		},
		cancelCh: make(chan struct{}),
	}
}

// newBatchID 生成一个随机任务 id。
func newBatchID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// batches 返回任务管理器(按需初始化,兼容测试里直接构造的 Server)。
func (s *Server) batches() *batchManager {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	if s.batchJobs == nil {
		s.batchJobs = newBatchManager()
	}
	return s.batchJobs
}

// batchInterval 返回批量抓取间隔。
func (s *Server) batchInterval() time.Duration {
	if s.batchDelay > 0 {
		return s.batchDelay
	}
	return defaultBatchDelay
}

// SetBatchDelay 设置批量抓取两篇之间的间隔。
func (s *Server) SetBatchDelay(d time.Duration) {
	if d > 0 {
		s.batchDelay = d
	}
}

// saveArticleByURL 抓取并归档一篇文章(测试可替换 saveURL)。
func (s *Server) saveArticleByURL(rawURL string) (string, bool, *wxmp.WechatOfficialArticle, error) {
	if s.saveURL != nil {
		return s.saveURL(rawURL)
	}
	if s.saver == nil {
		return "", false, nil, fmt.Errorf("保存器未初始化")
	}
	return s.saver.SaveFromURL(rawURL)
}

// batchURL 兼容两种请求写法:"urls":["..."] 与 "urls":[{"url":"...","title":"..."}]。
type batchURL struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// UnmarshalJSON 允许字符串或对象两种形式。
func (b *batchURL) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		b.URL = s
		return nil
	}
	var obj struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return err
	}
	b.URL = obj.URL
	b.Title = obj.Title
	return nil
}

// handleBatchCreate 接收一组文章链接,创建后台批量下载任务。
// 立即返回任务 id,进度通过 /api/batch/status 查询,打包通过 /api/batch/zip 下载。
func (s *Server) handleBatchCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBatchBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取请求失败"})
		return
	}
	var req struct {
		URLs     []batchURL `json:"urls"`
		Download bool       `json:"download"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式错误"})
		return
	}

	items, dropped := normalizeBatchURLs(req.URLs)
	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有可下载的文章链接"})
		return
	}
	if len(items) > maxBatchURLs {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("一次最多 %d 篇,当前 %d 篇", maxBatchURLs, len(items)),
		})
		return
	}

	job := newBatchJob(items)
	s.batches().add(job)
	go s.runBatch(job)

	s.logger.Info().Str("job", job.ID()).Int("total", len(items)).Int("dropped", dropped).Msg("创建批量下载任务")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "job_id": job.ID(), "total": len(items), "dropped": dropped,
	})
}

// normalizeBatchURLs 清洗、去重并过滤非法链接,保持原有顺序。
func normalizeBatchURLs(in []batchURL) ([]BatchItem, int) {
	items := make([]BatchItem, 0, len(in))
	seen := map[string]bool{}
	dropped := 0
	for _, raw := range in {
		rawURL := strings.TrimSpace(raw.URL)
		if !isHTTPURL(rawURL) || seen[rawURL] {
			if strings.TrimSpace(raw.URL) != "" {
				dropped++
			}
			continue
		}
		seen[rawURL] = true
		items = append(items, BatchItem{
			URL:   rawURL,
			Title: strings.TrimSpace(raw.Title),
			State: batchStatePending,
		})
	}
	return items, dropped
}

// isHTTPURL 只接受 http/https 链接,避免把奇怪协议传给抓取器。
func isHTTPURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// batchSaveFunc 归档一条链接,返回目录、标题、图片数、是否重复与错误。
type batchSaveFunc func(ctx context.Context, url string) (dir, title string, images int, duplicate bool, err error)

// runBatch 顺序抓取并归档任务里的公众号文章。
func (s *Server) runBatch(job *BatchJob) {
	s.runBatchFunc(job, func(_ context.Context, url string) (string, string, int, bool, error) {
		dir, duplicate, article, err := s.saveArticleByURL(url)
		title := ""
		if article != nil {
			title = article.Title
		}
		return dir, title, 0, duplicate, err
	})
}

// runBatchFunc 顺序执行批量任务。
func (s *Server) runBatchFunc(job *BatchJob, save batchSaveFunc) {
	total := len(job.snapshot().Items)
	interval := s.batchInterval()
	// 取消任务时同时取消当前条目的抓取/下载,而不是等它自己结束。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-job.cancelCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	for i := 0; i < total; i++ {
		if job.canceledFlag() {
			break
		}
		job.mu.Lock()
		url := job.snap.Items[i].URL
		job.mu.Unlock()

		job.setRunning(i)
		dir, title, images, duplicate, err := save(ctx, url)
		switch {
		case err != nil:
			s.logger.Error().Str("job", job.ID()).Str("url", url).Err(err).Msg("批量下载失败")
			job.finishItem(i, batchStateFailed, "", "", err.Error())
		case duplicate:
			s.logger.Info().Str("job", job.ID()).Str("url", url).Str("dir", dir).Msg("批量下载跳过(已存在)")
			job.finishItem(i, batchStateSkipped, dir, title, "")
		default:
			s.logger.Info().Str("job", job.ID()).Str("title", title).Str("dir", dir).Msg("批量下载成功")
			job.finishItem(i, batchStateOK, dir, title, "")
		}
		job.setItemImages(i, images)

		if i < total-1 && !job.canceledFlag() {
			timer := time.NewTimer(interval)
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

// handleBatchStatus 返回批量任务进度。
func (s *Server) handleBatchStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.findBatch(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "任务不存在或已过期"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job.snapshot()})
}

// handleBatchCancel 请求取消任务。
func (s *Server) handleBatchCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	job, ok := s.findBatch(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "任务不存在或已过期"})
		return
	}
	job.cancel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// findBatch 从查询参数 id 取任务。
func (s *Server) findBatch(r *http.Request) (*BatchJob, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		return nil, false
	}
	return s.batches().get(id)
}

// handleBatchZip 把任务里成功归档的文章打包成 ZIP 返回。
func (s *Server) handleBatchZip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	job, ok := s.findBatch(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "任务不存在或已过期"})
		return
	}
	// 允许任务仍在进行时下载已完成的部分,但至少要有内容。
	folders, manifest, count := buildBatchFolders(job.snapshot())
	if count == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有可打包的文章"})
		return
	}

	tmp, err := os.CreateTemp("", "wx-mp-batch-*.zip")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "创建临时文件失败: " + err.Error()})
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	defer tmp.Close()

	if err := writeZipArchive(tmp, folders, []zipFile{{Name: "_manifest.json", Data: manifest}}); err != nil {
		s.logger.Error().Err(err).Msg("批量打包失败")
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
	name := fmt.Sprintf("wx-mp-batch-%s.zip", snap.CreatedAt.Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(name))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition, X-Batch-Ok, X-Batch-Total")
	w.Header().Set("X-Batch-Ok", fmt.Sprintf("%d", count))
	w.Header().Set("X-Batch-Total", fmt.Sprintf("%d", snap.Total))
	if _, err := io.Copy(w, tmp); err != nil {
		s.logger.Warn().Err(err).Msg("输出批量 ZIP 失败")
	}
}

// buildBatchFolders 由任务快照生成 ZIP 目录列表与清单文件。
func buildBatchFolders(snap BatchSnapshot) ([]zipFolder, []byte, int) {
	var folders []zipFolder
	used := map[string]bool{}
	type manifestItem struct {
		URL    string `json:"url"`
		Title  string `json:"title,omitempty"`
		Folder string `json:"folder,omitempty"`
		State  string `json:"state"`
		Error  string `json:"error,omitempty"`
	}
	manifestItems := make([]manifestItem, 0, len(snap.Items))

	for _, item := range snap.Items {
		if (item.State != batchStateOK && item.State != batchStateSkipped) || item.Dir == "" {
			manifestItems = append(manifestItems, manifestItem{URL: item.URL, Title: item.Title, State: item.State, Error: item.Error})
			continue
		}
		// 目录可能已被用户删除,跳过但仍在清单里标注。
		if st, err := os.Stat(item.Dir); err != nil || !st.IsDir() {
			manifestItems = append(manifestItems, manifestItem{
				URL: item.URL, Title: item.Title, State: item.State, Error: "归档目录不存在",
			})
			continue
		}
		// 优先用归档元数据里的真实标题,页面上的结果标题只作兜底。
		title := readMetaTitle(item.Dir)
		if strings.TrimSpace(title) == "" {
			title = strings.TrimSpace(item.Title)
		}
		if strings.TrimSpace(title) == "" {
			title = filepath.Base(item.Dir)
		}
		folder := uniqueFolderName(sanitizeFileName(title), used)
		folders = append(folders, zipFolder{Prefix: folder, Dir: item.Dir})
		manifestItems = append(manifestItems, manifestItem{
			URL: item.URL, Title: title, Folder: folder, State: item.State,
		})
	}

	manifest, err := json.MarshalIndent(map[string]any{
		"job_id":     snap.ID,
		"created_at": snap.CreatedAt.Format(time.RFC3339),
		"total":      snap.Total,
		"ok":         snap.OK,
		"skipped":    snap.Skipped,
		"failed":     snap.Failed,
		"items":      manifestItems,
	}, "", "  ")
	if err != nil {
		manifest = []byte("{}")
	}
	return folders, manifest, len(folders)
}

// uniqueFolderName 处理 ZIP 内同名目录,自动追加序号。
func uniqueFolderName(base string, used map[string]bool) string {
	if base == "" {
		base = "article"
	}
	name := base
	for i := 2; used[strings.ToLower(name)]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[strings.ToLower(name)] = true
	return name
}
