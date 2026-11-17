package server

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"wx-mp-sync/internal/sync"
)

// maxDownloadBody 是下载请求体的上限(与保存接口一致)。
const maxDownloadBody = 16 << 20

// handleDownload 把当前页面文章(HTML + Markdown + 图片)归档后打包成 ZIP,
// 直接作为响应返回给浏览器下载。复用保存逻辑,已归档的文章直接复用现有文件。
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDownloadBody))
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
		s.logger.Error().Str("url", req.URL).Err(err).Msg("下载文章失败")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if dir == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "文章已存在,但找不到归档目录"})
		return
	}

	title := req.Title
	if article != nil && strings.TrimSpace(article.Title) != "" {
		title = article.Title
	}
	s.logger.Info().Str("url", req.URL).Str("dir", dir).Bool("duplicate", dup).Msg("下载文章 ZIP")
	s.writeArticleZip(w, dir, title, dup)
}

// handleDownloadSaved 把已归档的文章目录打包下载,供管理页「下载 ZIP」使用。
func (s *Server) handleDownloadSaved(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	dir, err := s.resolveArticleDir(r.URL.Query().Get("dir"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.writeArticleZip(w, dir, readMetaTitle(dir), false)
}

// writeArticleZip 把 dir 下的文章文件打包为 ZIP 并写入响应。
// 先打包到临时文件,确保失败时还能返回 JSON 错误而不是半截响应。
func (s *Server) writeArticleZip(w http.ResponseWriter, dir, title string, duplicate bool) {
	tmp, err := os.CreateTemp("", "wx-mp-sync-*.zip")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "创建临时文件失败: " + err.Error()})
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := writeZipDir(tmp, dir); err != nil {
		tmp.Close()
		s.logger.Error().Str("dir", dir).Err(err).Msg("打包文章失败")
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "打包失败: " + err.Error()})
		return
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		tmp.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "打包失败: " + err.Error()})
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "打包失败: " + err.Error()})
		return
	}
	defer tmp.Close()

	name := zipFileName(title)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(name))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition, X-Article-Duplicate")
	if duplicate {
		w.Header().Set("X-Article-Duplicate", "1")
	}
	if _, err := io.Copy(w, tmp); err != nil {
		// 响应头已发出,只能记录日志(通常是浏览器取消下载)。
		s.logger.Warn().Err(err).Msg("输出 ZIP 失败")
	}
}

// zipFolder 描述 ZIP 里的一个子目录及其来源目录;Prefix 为空时文件写在 ZIP 根目录。
type zipFolder struct {
	Prefix string
	Dir    string
}

// zipFile 描述一个直接写入 ZIP 的文件(如批量任务的清单)。
type zipFile struct {
	Name string
	Data []byte
}

// writeZipDir 把 dir 目录下的所有普通文件写入 ZIP(writer 由 zip 流式写出)。
func writeZipDir(dst io.Writer, dir string) error {
	return writeZipArchive(dst, []zipFolder{{Dir: dir}}, nil)
}

// writeZipArchive 把多个目录(可各自带 ZIP 内前缀)和额外文件写进同一个 ZIP。
func writeZipArchive(dst io.Writer, folders []zipFolder, files []zipFile) error {
	zw := zip.NewWriter(dst)

	for _, folder := range folders {
		base := filepath.Clean(folder.Dir)
		prefix := strings.Trim(filepath.ToSlash(folder.Prefix), "/")
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(base, path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			hdr, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(rel)
			if prefix != "" {
				name = prefix + "/" + name
			}
			hdr.Name = name
			hdr.Method = zip.Deflate

			out, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, in)
			closeErr := in.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		})
		if err != nil {
			_ = zw.Close()
			return err
		}
	}

	for _, file := range files {
		hdr := &zip.FileHeader{Name: strings.Trim(file.Name, "/"), Method: zip.Deflate}
		hdr.SetModTime(time.Now())
		out, err := zw.CreateHeader(hdr)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if _, err := out.Write(file.Data); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}

// resolveArticleDir 把前端传来的相对目录安全地解析到输出目录下,
// 拒绝绝对路径与 ../ 穿越,并要求目录里确实是一篇文章。
func (s *Server) resolveArticleDir(rel string) (string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "" {
		return "", fmt.Errorf("缺少 dir 参数")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("非法目录")
	}

	root, err := filepath.Abs(s.outputDir)
	if err != nil {
		return "", fmt.Errorf("非法目录")
	}
	full := filepath.Join(root, clean)
	relCheck, err := filepath.Rel(root, full)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("非法目录")
	}

	st, err := os.Stat(full)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("文章目录不存在")
	}
	if _, err := os.Stat(filepath.Join(full, "metadata.json")); err != nil {
		if _, err := os.Stat(filepath.Join(full, "index.html")); err != nil {
			return "", fmt.Errorf("不是文章目录")
		}
	}
	return full, nil
}

// readMetaTitle 读取归档目录 metadata.json 里的标题,失败时返回空串。
func readMetaTitle(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.Title)
}

// zipFileName 由文章标题生成 ZIP 文件名。
func zipFileName(title string) string {
	name := sanitizeFileName(title)
	if name == "" {
		name = "wechat-article"
	}
	return name + ".zip"
}

// sanitizeFileName 去掉 Windows 文件名非法字符,并限制长度。
func sanitizeFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|', '\r', '\n', '\t':
			return '_'
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ".")
	if utf8.RuneCountInString(s) > 80 {
		s = string([]rune(s)[:80])
	}
	return strings.TrimSpace(s)
}

// contentDisposition 生成兼容中文文件名的下载头(ASCII 兜底 + RFC 5987)。
func contentDisposition(name string) string {
	fallback := "article.zip"
	if isASCII(name) {
		fallback = name
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, fallback, url.PathEscape(name))
}

// isASCII 判断字符串是否全部为 ASCII 字符。
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
