// Package server 提供本地 Web 管理服务:接收注入按钮的保存请求、展示已同步文章、手动同步、查看日志。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	gosync "sync"
	"time"

	"github.com/rs/zerolog"

	"wx-mp-sync/internal/bot"
	"wx-mp-sync/internal/capture"
	"wx-mp-sync/internal/naicha"
	"wx-mp-sync/internal/sync"
	"wx-mp-sync/pkg/scraper/wxmp"
	"wx-mp-sync/pkg/scraper/xhs"
)

//go:embed admin.html
var adminFS embed.FS

// Status 描述运行状态,由 app 编排层填充。
type Status struct {
	Version     string `json:"version"`
	ProxyAddr   string `json:"proxy_addr"`
	AdminAddr   string `json:"admin_addr"`
	OutputDir   string `json:"output_dir"`
	CertName    string `json:"cert_name"`
	SystemProxy string `json:"system_proxy"`
	BotAPI      string `json:"bot_api"`      // bot 推送地址,未启用为空
	NaichaAPI   string `json:"naicha_api"`   // 奶茶圈论坛地址,未启用为空
	NaichaAuto  bool   `json:"naicha_auto"`  // 保存后是否自动同步到论坛
	NaichaBoard string `json:"naicha_board"` // 小红书同步使用的 boardSlug
	XHSEnabled  bool   `json:"xhs_enabled"`  // 是否可同步小红书到奶茶圈
	// XHSDownloadEnabled 表示是否可以把小红书笔记批量下载到本地。
	XHSDownloadEnabled bool `json:"xhs_download_enabled"`
}

// ArticleInfo 是已同步文章列表中的一项。
type ArticleInfo struct {
	Title       string `json:"title"`
	Author      string `json:"author"`
	PublishTime string `json:"publish_time"`
	URL         string `json:"url"`
	Dir         string `json:"dir"` // 相对输出目录,用于前端链接
	HTML        string `json:"html"`
	Markdown    string `json:"markdown"`
	SyncedAt    string `json:"synced_at"`
	NaichaURL   string `json:"naicha_url,omitempty"` // 已发布到论坛的帖子地址
	NaichaAt    string `json:"naicha_at,omitempty"`  // 发布到论坛的时间
}

// Server 是本地管理服务。
type Server struct {
	outputDir string
	logPath   string
	status    *Status
	saver     *sync.ArticleSaver
	botSyncer *bot.Syncer    // 可为 nil,不推送
	naicha    *naicha.Client // 可为 nil,不发布到论坛
	logger    *zerolog.Logger
	http      *http.Server

	batchMu    gosync.Mutex
	batchJobs  *batchManager
	batchDelay time.Duration // 批量抓取间隔,0 表示用默认值
	// saveURL 允许测试替换单篇抓取逻辑;为 nil 时走 saver。
	saveURL func(string) (string, bool, *wxmp.WechatOfficialArticle, error)

	capture       *capture.Store
	accountClient *wxmp.AccountClient
	// fetchLatest 允许测试替换 getmsg 拉取逻辑。
	fetchLatest func(*wxmp.OfficialAccount, int) ([]wxmp.FeedArticle, error)

	// 豆包(m.doubao.com)分享批量下载。
	doubaoMu       gosync.Mutex
	doubaoJobs     *doubaoManager
	doubaoArchiver doubaoArchiver

	// ChatGPT(chatgpt.com)分享批量下载。
	chatgptArchiver chatgptArchiver

	// 小红书笔记 → 奶茶圈帖子。
	xhsScraper *xhs.Client
	xhsBoard   string
	xhsTags    []string
	// 小红书笔记本地归档。
	xhsArchiver xhsNoteArchiver
}

// New 创建管理服务。botSyncer 为 nil 时不进行 bot 推送。
func New(outputDir, logPath string, saver *sync.ArticleSaver, botSyncer *bot.Syncer, logger *zerolog.Logger) *Server {
	return &Server{
		outputDir:     outputDir,
		logPath:       logPath,
		status:        &Status{OutputDir: outputDir, CertName: "wx-mp-sync"},
		saver:         saver,
		botSyncer:     botSyncer,
		logger:        logger,
		accountClient: wxmp.NewAccountClient(logger),
	}
}

// SetCapture 注入公众号凭证存储(用于「公众号最新文章」)。
func (s *Server) SetCapture(store *capture.Store) {
	s.capture = store
}

// SetNaicha 注入奶茶圈论坛同步客户端;传 nil 表示不启用。
func (s *Server) SetNaicha(client *naicha.Client) {
	s.naicha = client
}

// SetXHS 注入小红书抓取器,并设置发帖分类和标签。
func (s *Server) SetXHS(scraper *xhs.Client, board string, tags []string) {
	s.xhsScraper = scraper
	s.xhsBoard = strings.TrimSpace(board)
	s.xhsTags = tags
}

// SetStatus 由编排层填充运行状态。
func (s *Server) SetStatus(st *Status) {
	if st != nil {
		s.status = st
	}
}

// Handler 返回带全部路由与 CORS 的处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/save", s.withCORS(s.handleSave))
	mux.HandleFunc("/api/sync", s.withCORS(s.handleSync))
	mux.HandleFunc("/api/download", s.withCORS(s.handleDownload))
	mux.HandleFunc("/api/download/saved", s.withCORS(s.handleDownloadSaved))
	mux.HandleFunc("/api/batch", s.withCORS(s.handleBatchCreate))
	mux.HandleFunc("/api/batch/status", s.withCORS(s.handleBatchStatus))
	mux.HandleFunc("/api/batch/cancel", s.withCORS(s.handleBatchCancel))
	mux.HandleFunc("/api/batch/zip", s.withCORS(s.handleBatchZip))
	mux.HandleFunc("/api/doubao/batch", s.withCORS(s.handleDoubaoBatchCreate))
	mux.HandleFunc("/api/doubao/status", s.withCORS(s.handleDoubaoStatus))
	mux.HandleFunc("/api/doubao/cancel", s.withCORS(s.handleDoubaoCancel))
	mux.HandleFunc("/api/doubao/zip", s.withCORS(s.handleDoubaoZip))
	mux.HandleFunc("/api/doubao/one", s.withCORS(s.handleDoubaoOne))
	mux.HandleFunc("/api/chatgpt/batch", s.withCORS(s.handleChatGPTBatchCreate))
	mux.HandleFunc("/api/chatgpt/one", s.withCORS(s.handleChatGPTOne))
	mux.HandleFunc("/api/chatgpt/status", s.withCORS(s.handleBatchStatus))
	mux.HandleFunc("/api/chatgpt/cancel", s.withCORS(s.handleBatchCancel))
	mux.HandleFunc("/api/chatgpt/zip", s.withCORS(s.handleBatchZip))
	mux.HandleFunc("/api/accounts", s.withCORS(s.handleAccounts))
	mux.HandleFunc("/api/accounts/latest", s.withCORS(s.handleAccountLatest))
	mux.HandleFunc("/api/naicha/sync", s.withCORS(s.handleNaichaSync))
	mux.HandleFunc("/api/xhs/sync", s.withCORS(s.handleXHSSync))
	mux.HandleFunc("/api/xhs/batch", s.withCORS(s.handleXHSBatchCreate))
	mux.HandleFunc("/api/xhs/status", s.withCORS(s.handleBatchStatus))
	mux.HandleFunc("/api/xhs/cancel", s.withCORS(s.handleBatchCancel))
	mux.HandleFunc("/api/xhs/zip", s.withCORS(s.handleBatchZip))
	mux.HandleFunc("/api/diag", s.withCORS(s.handleDiag))
	mux.HandleFunc("/api/list", s.withCORS(s.handleList))
	mux.HandleFunc("/api/logs", s.withCORS(s.handleLogs))
	mux.HandleFunc("/api/status", s.withCORS(s.handleStatus))
	mux.HandleFunc("/", s.handleAdmin)
	mux.Handle("/articles/", http.StripPrefix("/articles/", http.FileServer(http.Dir(s.outputDir))))
	return mux
}

// Start 在指定端口启动管理服务。
func (s *Server) Start(port int) error {
	s.http = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: s.Handler(),
	}
	go func() {
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error().Err(err).Msg("管理服务错误")
		}
	}()
	return nil
}

// Stop 优雅关闭管理服务。
func (s *Server) Stop() error {
	if s.http == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.http.Shutdown(ctx)
	s.http = nil
	return err
}

// withCORS 为 API 增加跨域头,并处理 OPTIONS 预检。
func (s *Server) withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		// Chrome 的 Private Network Access:公网页面 fetch 127.0.0.1 需要显式允许。
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition, X-Article-Duplicate, X-Batch-Ok, X-Batch-Total")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleSave 接收注入按钮发来的文章数据并归档。
func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取请求失败"})
		return
	}
	var req sync.SaveRequest
	if err := json.Unmarshal(body, &req); err != nil || req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 url 或请求格式错误"})
		return
	}

	dir, dup, article, err := s.saver.SaveFromCgiData(req.URL, req.CgiDataNew)
	if err != nil {
		s.logger.Error().Str("url", req.URL).Err(err).Msg("保存文章失败")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.logger.Info().Str("url", req.URL).Str("dir", dir).Bool("duplicate", dup).Msg("保存文章成功")

	// 记录公众号昵称,供「最新文章」列表展示。
	if s.capture != nil && req.Nickname != "" {
		if biz := capture.BizFromURL(req.URL); biz != "" {
			s.capture.SetNickname(biz, req.Nickname)
		}
	}

	var botResult *bot.SyncResult
	// 「⬆ 发帖」只发布到论坛,不重复推送 bot;需要 bot 推送请用「⬇ 同步」。
	if r.URL.Query().Get("naicha") != "1" {
		botResult = s.pushToBot(r, article, req.URL)
	}
	var naichaResult *naicha.Result
	// 注入页面的「发帖」按钮通过 ?naicha=1 强制发布,不受 auto 配置影响。
	if r.URL.Query().Get("naicha") == "1" {
		naichaResult = s.pushToNaicha(r.Context(), dir, false)
	} else {
		naichaResult = s.autoPushToNaicha(r.Context(), dir)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "duplicate": dup, "dir": dir, "bot": botResult, "naicha": naichaResult,
	})
}

// pushToBot 在保存成功后把文章推送到 bot 服务(未启用或文章为空时返回 nil)。
// rawURL 为用户请求里的原始文章链接(注入按钮或手动同步)。
func (s *Server) pushToBot(r *http.Request, article *wxmp.WechatOfficialArticle, rawURL string) *bot.SyncResult {
	if s.botSyncer == nil || article == nil {
		return nil
	}
	res := s.botSyncer.Sync(r.Context(), article, rawURL)
	return &res
}

// pushToNaicha 把指定目录发布到奶茶圈;未启用时返回 nil。
func (s *Server) pushToNaicha(ctx context.Context, dir string, force bool) *naicha.Result {
	if s.naicha == nil || strings.TrimSpace(dir) == "" {
		return nil
	}
	res := s.naicha.SyncDir(ctx, dir, force)
	return &res
}

// autoPushToNaicha 在启用 auto 时把刚保存的文章同步到奶茶圈;否则返回 nil。
func (s *Server) autoPushToNaicha(ctx context.Context, dir string) *naicha.Result {
	if s.naicha == nil || !s.naicha.Auto() {
		return nil
	}
	return s.pushToNaicha(ctx, dir, false)
}

// handleNaichaSync 是「同步文章」功能:按归档目录或文章链接发布到奶茶圈论坛。
// 传 url 时会先归档(复用已有归档),再发布。
func (s *Server) handleNaichaSync(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		Dir   string `json:"dir"`
		URL   string `json:"url"`
		Force bool   `json:"force"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式错误"})
		return
	}

	dir := strings.TrimSpace(req.Dir)
	rawURL := strings.TrimSpace(req.URL)
	if dir == "" && rawURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 dir 或 url"})
		return
	}

	if dir == "" {
		if !isHTTPPageURL(rawURL) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "只支持 http(s) 文章链接"})
			return
		}
		// 已经归档过的文章直接复用,避免再抓一次微信(链接过期时尤其有用)。
		if archived := s.findArchivedByURL(rawURL); archived != "" {
			dir = archived
		} else {
			if s.saver == nil {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "归档器未就绪"})
				return
			}
			articleDir, _, _, err := s.saver.SaveFromURL(rawURL)
			if err != nil {
				s.logger.Error().Str("url", rawURL).Err(err).Msg("同步到奶茶圈前归档失败")
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "归档失败: " + err.Error()})
				return
			}
			dir = articleDir
		}
	}

	resolved := ""
	abs, err := s.resolveArticleDir(dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if base, err := filepath.Abs(s.outputDir); err == nil {
		if rel, err := filepath.Rel(base, abs); err == nil {
			resolved = filepath.ToSlash(rel)
		}
	}
	if resolved == "" {
		resolved = filepath.ToSlash(dir)
	}

	result := s.naicha.SyncDir(r.Context(), abs, req.Force)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        result.OK,
		"duplicate": result.Duplicate,
		"id":        result.ID,
		"post_id":   result.PostID,
		"url":       result.URL,
		"title":     result.Title,
		"images":    result.Images,
		"truncated": result.Truncated,
		"error":     result.Message,
		"dir":       resolved,
	})
}

// findArchivedByURL 在已归档文章里按原文链接查找目录,找不到返回空串。
func (s *Server) findArchivedByURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	list, err := s.scanArticles()
	if err != nil {
		return ""
	}
	for _, item := range list {
		if strings.TrimSpace(item.URL) == rawURL {
			return item.Dir
		}
	}
	return ""
}

// isHTTPPageURL 判断是否为 http(s) 链接。
func isHTTPPageURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// handleSync 手动按 URL 同步(管理页输入框)。
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	var req sync.SaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 url"})
		return
	}
	dir, dup, article, err := s.saver.SaveFromURL(req.URL)
	if err != nil {
		s.logger.Error().Str("url", req.URL).Err(err).Msg("手动同步失败")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	botResult := s.pushToBot(r, article, req.URL)
	naichaResult := s.autoPushToNaicha(r.Context(), dir)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "duplicate": dup, "dir": dir, "bot": botResult, "naicha": naichaResult,
	})
}

// handleList 返回已同步文章列表。
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := s.scanArticles()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "list": []ArticleInfo{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "list": list})
}

// handleLogs 返回日志文件尾部内容。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	content := ""
	if s.logPath != "" {
		if b, err := tailFile(s.logPath, 64<<10); err == nil {
			content = b
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logs": content})
}

// handleStatus 返回运行状态。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.status})
}

// handleDiag 接收注入脚本上报的页面诊断信息,仅记录日志。
func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取失败"})
		return
	}
	// 顺带捕获公众号昵称(凭证由代理插件从请求 URL/Cookie 里捕获)。
	if s.capture != nil {
		var diag struct {
			Biz  string `json:"biz"`
			Nick string `json:"nick"`
		}
		if json.Unmarshal(body, &diag) == nil && diag.Biz != "" && diag.Nick != "" {
			s.capture.SetNickname(diag.Biz, diag.Nick)
		}
	}
	s.logger.Info().Str("diag", string(body)).Msg("页面诊断")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdmin 返回内嵌的管理页面。
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := adminFS.ReadFile("admin.html")
	if err != nil {
		http.Error(w, "admin page missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// scanArticles 递归扫描输出目录下的 metadata.json 组装文章列表。
func (s *Server) scanArticles() ([]ArticleInfo, error) {
	var list []ArticleInfo
	err := filepath.WalkDir(s.outputDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() != "metadata.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var meta struct {
			Title       string `json:"title"`
			Author      string `json:"author"`
			PublishTime string `json:"publish_time"`
			URL         string `json:"url"`
			HTML        string `json:"html"`
			Markdown    string `json:"markdown"`
			SyncedAt    string `json:"synced_at"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil
		}
		dir, err := filepath.Rel(s.outputDir, filepath.Dir(path))
		if err != nil {
			dir = filepath.Dir(path)
		}
		list = append(list, ArticleInfo{
			Title:       meta.Title,
			Author:      meta.Author,
			PublishTime: meta.PublishTime,
			URL:         meta.URL,
			Dir:         filepath.ToSlash(dir),
			HTML:        meta.HTML,
			Markdown:    meta.Markdown,
			SyncedAt:    meta.SyncedAt,
		})
		if s.naicha != nil {
			if absDir, err := filepath.Abs(filepath.Dir(path)); err == nil {
				if rec, ok := s.naicha.Lookup(absDir); ok {
					list[len(list)-1].NaichaURL = rec.URL
					list[len(list)-1].NaichaAt = rec.SyncedAt
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].SyncedAt > list[j].SyncedAt
	})
	return list, nil
}

// tailFile 返回文件末尾最多 maxBytes 字节的内容。
func tailFile(path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := st.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
