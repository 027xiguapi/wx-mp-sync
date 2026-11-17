package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"

	chatgptarchive "wx-mp-sync/internal/chatgpt"
	scraper "wx-mp-sync/pkg/scraper/chatgpt"
)

const (
	// maxChatGPTURLs 是单次 ChatGPT 批量下载允许的链接数上限。
	maxChatGPTURLs = 50
	// maxChatGPTBody 是请求体上限。
	maxChatGPTBody = 1 << 20
)

// chatgptArchiver 是 ChatGPT 归档器接口,便于测试替换。
type chatgptArchiver interface {
	Save(ctx context.Context, rawURL string) (*chatgptarchive.Result, error)
}

// SetChatGPTArchiver 注入 ChatGPT 归档器。
func (s *Server) SetChatGPTArchiver(a chatgptArchiver) {
	s.chatgptArchiver = a
}

// handleChatGPTBatchCreate 接收一组 ChatGPT 分享链接,创建后台任务。
// 任务进度与打包复用 /api/batch/status、/api/batch/zip。
func (s *Server) handleChatGPTBatchCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxChatGPTBody))
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
	items, dropped := normalizeChatGPTURLs(req.URLs)
	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有可下载的 ChatGPT 分享链接"})
		return
	}
	if len(items) > maxChatGPTURLs {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "一次最多 50 个链接",
		})
		return
	}
	if s.chatgptArchiver == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "ChatGPT 归档器未初始化"})
		return
	}

	job := newBatchJob(items)
	s.batches().add(job)
	go s.runBatchFunc(job, func(ctx context.Context, rawURL string) (string, string, int, bool, error) {
		result, err := s.chatgptArchiver.Save(ctx, rawURL)
		if err != nil {
			return "", "", 0, false, err
		}
		return result.Dir, result.Title, result.Images, result.Duplicate, nil
	})

	s.logger.Info().Str("job", job.ID()).Int("total", len(items)).Int("dropped", dropped).Msg("创建 ChatGPT 批量下载任务")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "job_id": job.ID(), "total": len(items), "dropped": dropped,
	})
}

// normalizeChatGPTURLs 过滤非法/重复链接,按分享 id 去重。
func normalizeChatGPTURLs(in []batchURL) ([]BatchItem, int) {
	items := make([]BatchItem, 0, len(in))
	seen := map[string]bool{}
	dropped := 0
	for _, raw := range in {
		rawURL := strings.TrimSpace(raw.URL)
		id, err := scraper.ParseShareID(rawURL)
		if err != nil {
			if rawURL != "" {
				dropped++
			}
			continue
		}
		if seen[id] {
			dropped++
			continue
		}
		seen[id] = true
		items = append(items, BatchItem{
			URL:   scraper.CanonicalURL(id),
			Title: strings.TrimSpace(raw.Title),
			State: batchStatePending,
		})
	}
	return items, dropped
}

// handleChatGPTOne 归档单个 ChatGPT 分享并直接返回 ZIP(供注入按钮调用)。
// 页面 CSP 拦截 fetch 时,注入脚本会退化为顶层导航,此时错误以 HTML 返回。
func (s *Server) handleChatGPTOne(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		s.writeChatGPTOneError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rawURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			s.writeChatGPTOneError(w, r, http.StatusBadRequest, "读取请求失败")
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
		s.writeChatGPTOneError(w, r, http.StatusBadRequest, "缺少 url")
		return
	}
	if _, err := scraper.ParseShareID(rawURL); err != nil {
		s.writeChatGPTOneError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if s.chatgptArchiver == nil {
		s.writeChatGPTOneError(w, r, http.StatusServiceUnavailable, "ChatGPT 归档器未初始化")
		return
	}

	result, err := s.chatgptArchiver.Save(r.Context(), rawURL)
	if err != nil {
		s.logger.Error().Str("url", rawURL).Err(err).Msg("ChatGPT 单篇下载失败")
		s.writeChatGPTOneError(w, r, http.StatusOK, err.Error())
		return
	}
	s.logger.Info().Str("url", rawURL).Str("dir", result.Dir).Bool("duplicate", result.Duplicate).Msg("ChatGPT 单篇打包下载")
	s.writeArticleZip(w, result.Dir, result.Title, result.Duplicate)
}

// writeChatGPTOneError 普通请求返回 JSON;导航请求返回一个可返回上一页的 HTML 错误页。
func (s *Server) writeChatGPTOneError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	wantsHTML := r.URL.Query().Get("nav") == "1" ||
		strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
	if !wantsHTML {
		writeJSON(w, status, map[string]any{"ok": false, "error": msg})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>ChatGPT 下载失败</title>
<style>body{font-family:-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;max-width:680px;margin:60px auto;padding:0 20px;color:#333;line-height:1.7}
.err{background:#fff2f0;border:1px solid #ffccc7;border-radius:8px;padding:16px}
a{color:#10a37f}</style></head><body>
<h2>ChatGPT 分享下载失败</h2>
<div class="err">%s</div>
<p><a href="javascript:history.back()">返回分享页</a> · 也可以回到管理页把链接粘贴到「ChatGPT 分享下载」里重试。</p>
</body></html>`, html.EscapeString(msg))
}
