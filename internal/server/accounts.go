package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"wx-mp-sync/pkg/scraper/wxmp"
)

// accountView 是管理页展示用的账号信息(不返回会话凭证)。
type accountView struct {
	Biz       string `json:"biz"`
	Nickname  string `json:"nickname"`
	LastSeen  string `json:"last_seen"`
	Usable    bool   `json:"usable"`
	HasCookie bool   `json:"has_cookie"`
}

// latestArticleView 是「最新文章」列表里的一项。
type latestArticleView struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Cover    string `json:"cover"`
	Digest   string `json:"digest"`
	Author   string `json:"author"`
	DateTime string `json:"datetime"`
}

// handleAccounts 返回已捕获凭证的公众号列表。
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	views := []accountView{}
	if s.capture != nil {
		for _, acct := range s.capture.List() {
			views = append(views, accountView{
				Biz:       acct.Biz,
				Nickname:  acct.Nickname,
				LastSeen:  acct.LastSeen.Format(time.RFC3339),
				Usable:    acct.Usable,
				HasCookie: acct.Cookie != "",
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": views})
}

// handleAccountLatest 用捕获到的凭证拉取该公众号的最新文章列表。
func (s *Server) handleAccountLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	if s.capture == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "凭证捕获未启用"})
		return
	}
	biz := strings.TrimSpace(r.URL.Query().Get("biz"))
	if biz == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 biz 参数"})
		return
	}
	acct, ok := s.capture.Get(biz)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有捕获到该公众号,请先在微信里打开它的一篇文章"})
		return
	}
	if !acct.Usable {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": "该公众号的会话凭证不完整,请在微信里重新打开它的文章后再试",
		})
		return
	}

	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 50 {
		limit = 50
	}

	wxAcct := &wxmp.OfficialAccount{
		Name:        acct.Nickname,
		Biz:         acct.Biz,
		Uin:         acct.Uin,
		Key:         acct.Key,
		PassTicket:  acct.PassTicket,
		Cookie:      acct.Cookie,
		MaxArticles: limit,
	}
	items, err := s.latestArticles(wxAcct, limit)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "no session") || strings.Contains(msg, "ret=-3") {
			msg = "微信会话已失效:请在微信里重新打开该公众号的文章(或历史消息)后再试"
		}
		s.logger.Warn().Str("biz", biz).Err(err).Msg("拉取公众号最新文章失败")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": msg})
		return
	}

	views := make([]latestArticleView, 0, len(items))
	for _, item := range items {
		view := latestArticleView{
			Title:  item.Title,
			URL:    item.URL,
			Cover:  item.Cover,
			Digest: item.Digest,
			Author: item.Author,
		}
		if item.Datetime > 0 {
			view.DateTime = time.Unix(item.Datetime, 0).Format("2006-01-02 15:04")
		}
		views = append(views, view)
	}
	s.logger.Info().Str("biz", biz).Int("count", len(views)).Msg("拉取公众号最新文章成功")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"count": len(views),
		"items": views,
		"account": accountView{
			Biz: acct.Biz, Nickname: acct.Nickname,
			LastSeen: acct.LastSeen.Format(time.RFC3339), Usable: acct.Usable, HasCookie: acct.Cookie != "",
		},
	})
}

// latestArticles 调用 getmsg 拉取最新文章;测试可替换 fetchLatest。
func (s *Server) latestArticles(acct *wxmp.OfficialAccount, limit int) ([]wxmp.FeedArticle, error) {
	if s.fetchLatest != nil {
		return s.fetchLatest(acct, limit)
	}
	if s.accountClient == nil {
		s.accountClient = wxmp.NewAccountClient(s.logger)
	}
	return s.accountClient.FetchLatestArticles(acct, limit)
}
