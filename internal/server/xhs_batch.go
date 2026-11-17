package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	xhsarchive "wx-mp-sync/internal/xhsarchive"
	"wx-mp-sync/pkg/scraper/xhs"
)

const (
	// maxXHSBatchBody 是小红书批量下载请求体的上限。
	maxXHSBatchBody = 1 << 20
)

// xhsNoteArchiver 是小红书本地归档器接口,便于测试替换。
type xhsNoteArchiver interface {
	Save(ctx context.Context, rawURL string) (*xhsarchive.Result, error)
}

// SetXHSArchiver 注入小红书本地归档器。
func (s *Server) SetXHSArchiver(a xhsNoteArchiver) {
	s.xhsArchiver = a
}

// handleXHSBatchCreate 接收一组小红书笔记链接,创建后台下载任务。
// 进度与 ZIP 打包复用 /api/batch/status、/api/batch/zip 的实现。
func (s *Server) handleXHSBatchCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxXHSBatchBody))
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

	items, dropped := normalizeXHSBatchURLs(req.URLs)
	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有可下载的小红书笔记链接"})
		return
	}
	if len(items) > maxBatchURLs {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "一次最多 100 篇小红书笔记",
		})
		return
	}
	if s.xhsArchiver == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "小红书本地归档器未初始化"})
		return
	}

	job := newBatchJob(items)
	s.batches().add(job)
	go s.runBatchFunc(job, func(ctx context.Context, rawURL string) (string, string, int, bool, error) {
		result, err := s.xhsArchiver.Save(ctx, rawURL)
		if err != nil {
			return "", "", 0, false, err
		}
		return result.Dir, result.Title, result.Images, result.Duplicate, nil
	})

	s.logger.Info().Str("job", job.ID()).Int("total", len(items)).Int("dropped", dropped).Msg("创建小红书批量下载任务")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "job_id": job.ID(), "total": len(items), "dropped": dropped,
	})
}

// normalizeXHSBatchURLs 从每行文本里提取小红书链接,过滤非法/重复链接。
// 完整链接里的 xsec_token 必须保留,否则分享链接可能无法访问。
func normalizeXHSBatchURLs(in []batchURL) ([]BatchItem, int) {
	items := make([]BatchItem, 0, len(in))
	seen := map[string]bool{}
	dropped := 0
	for _, raw := range in {
		rawURL := xhs.ExtractURL(raw.URL)
		if rawURL == "" {
			if strings.TrimSpace(raw.URL) != "" {
				dropped++
			}
			continue
		}
		key := rawURL
		if id, err := xhs.ParseNoteID(rawURL); err == nil {
			key = id
		}
		if seen[key] {
			dropped++
			continue
		}
		seen[key] = true
		items = append(items, BatchItem{
			URL:   rawURL,
			Title: strings.TrimSpace(raw.Title),
			State: batchStatePending,
		})
	}
	return items, dropped
}
