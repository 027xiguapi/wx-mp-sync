// Package server 提供本地 Web 管理服务:接收注入按钮的保存请求、展示已同步文章、手动同步、查看日志。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rs/zerolog"

	"wx-mp-sync/internal/bot"
	"wx-mp-sync/internal/sync"
	"wx-mp-sync/pkg/scraper/wxmp"
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
	BotAPI      string `json:"bot_api"` // bot 推送地址,未启用为空
}

// ArticleInfo 是已同步文章列表中的一项。
type ArticleInfo struct {
	Title       string `json:"title"`
	Author      string `json:"author"`
	PublishTime string `json:"publish_time"`
	URL         string `json:"url"`
	Dir         string `json:"dir"` // 相对输出目录,用于前端链接
	HTML        string `json:"html"`
	SyncedAt    string `json:"synced_at"`
}

// Server 是本地管理服务。
type Server struct {
	outputDir string
	logPath   string
	status    *Status
	saver     *sync.ArticleSaver
	botSyncer *bot.Syncer // 可为 nil,不推送
	logger    *zerolog.Logger
	http      *http.Server
}

// New 创建管理服务。botSyncer 为 nil 时不进行 bot 推送。
func New(outputDir, logPath string, saver *sync.ArticleSaver, botSyncer *bot.Syncer, logger *zerolog.Logger) *Server {
	return &Server{
		outputDir: outputDir,
		logPath:   logPath,
		status:    &Status{OutputDir: outputDir, CertName: "wx-mp-sync"},
		saver:     saver,
		botSyncer: botSyncer,
		logger:    logger,
	}
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

	botResult := s.pushToBot(r, article, req.URL)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "duplicate": dup, "dir": dir, "bot": botResult,
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
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "duplicate": dup, "dir": dir, "bot": botResult,
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
			SyncedAt:    meta.SyncedAt,
		})
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
