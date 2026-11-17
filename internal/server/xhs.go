package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"wx-mp-sync/internal/xhssync"
	"wx-mp-sync/pkg/scraper/xhs"
)

// handleXHSSync 把一篇小红书笔记同步到奶茶圈。
// 支持两种来源:
//  1. {"url":"https://www.xiaohongshu.com/explore/..."} 由本地服务抓取页面;
//  2. {"url":"...","note":{...}} 由小红书页面里的注入按钮直接提交笔记数据。
func (s *Server) handleXHSSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	if s.naicha == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": "没有启用奶茶圈同步:请在 config.yaml 的 naicha_sync 里设置 enabled: true",
		})
		return
	}
	if !s.naicha.SessionReady() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": "没有配置奶茶圈管理员登录:请在 naicha_sync 里填写 username / password(或 cookie)",
		})
		return
	}
	if s.xhsScraper == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "小红书抓取器未就绪"})
		return
	}

	var req struct {
		URL   string          `json:"url"`
		Note  json.RawMessage `json:"note"`
		Force bool            `json:"force"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式错误"})
		return
	}
	rawURL := strings.TrimSpace(req.URL)

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	var (
		note *xhs.Note
		err  error
	)
	if len(req.Note) > 0 && string(req.Note) != "null" {
		noteID := ""
		if id, idErr := xhs.ParseNoteID(rawURL); idErr == nil {
			noteID = id
		}
		note, err = xhs.ParseCapturedNote(req.Note, noteID)
		if err == nil {
			if strings.TrimSpace(note.URL) == "" {
				note.URL = rawURL
			}
			if strings.TrimSpace(note.URL) == "" && note.ID != "" {
				note.URL = xhs.CanonicalURL(note.ID)
			}
		}
	} else {
		if rawURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少笔记链接"})
			return
		}
		if !xhs.IsXHSURL(rawURL) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "只支持小红书笔记链接(xiaohongshu.com 或 xhslink.com)"})
			return
		}
		note, err = s.xhsScraper.Fetch(ctx, rawURL)
	}
	if err != nil {
		s.logger.Warn().Err(err).Str("url", rawURL).Msg("小红书笔记解析失败")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if note.ID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "没有解析到笔记 ID,无法去重和同步"})
		return
	}

	s.logger.Info().
		Str("note_id", note.ID).
		Str("title", note.Title).
		Int("images", len(note.Images)).
		Msg("同步小红书笔记到奶茶圈")

	result := xhssync.Sync(ctx, s.naicha, s.xhsScraper, note, xhssync.Options{
		Board: s.xhsBoard,
		Tags:  s.xhsTags,
	}, req.Force)

	errText := ""
	if !result.Publish.OK {
		errText = result.Publish.Message
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            result.Publish.OK,
		"duplicate":     result.Publish.Duplicate,
		"id":            result.Publish.ID,
		"url":           result.Publish.URL,
		"title":         result.Publish.Title,
		"images":        result.Images,
		"failed_images": result.FailedImages,
		"pending":       result.Publish.Pending,
		"note_id":       note.ID,
		"error":         errText,
	})
}
