package server

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"wx-mp-sync/internal/config"
	"wx-mp-sync/internal/store"
	"wx-mp-sync/internal/sync"
)

// TestWriteZipDir 验证目录打包后的文件清单与内容。
func TestWriteZipDir(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"index.html":        "<html>hi</html>",
		"文章.md":             "# 标题",
		"metadata.json":     `{"title":"标题"}`,
		"images/abcdef.jpg": "fake-image-bytes",
	}
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := writeZipDir(&buf, dir); err != nil {
		t.Fatalf("writeZipDir: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		got[f.Name] = string(data)
	}
	if len(got) != len(files) {
		t.Fatalf("zip 文件数 = %d, 期望 %d (%v)", len(got), len(files), got)
	}
	for name, want := range files {
		if got[name] != want {
			t.Errorf("%s 内容 = %q, 期望 %q", name, got[name], want)
		}
	}
}

// TestResolveArticleDir 验证路径穿越被拒绝、正常文章目录可解析。
func TestResolveArticleDir(t *testing.T) {
	root := t.TempDir()
	s := &Server{outputDir: root}

	article := filepath.Join(root, "公众号", "2026-10", "标题")
	if err := os.MkdirAll(article, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(article, "metadata.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := s.resolveArticleDir("公众号/2026-10/标题")
	if err != nil {
		t.Fatalf("resolveArticleDir: %v", err)
	}
	if got != article {
		t.Errorf("目录 = %q, 期望 %q", got, article)
	}

	for _, bad := range []string{"", "../", "..", "公众号/../../etc", filepath.Join(root, "公众号")} {
		if _, err := s.resolveArticleDir(bad); err == nil {
			t.Errorf("resolveArticleDir(%q) 应当失败", bad)
		}
	}
}

// TestZipFileName 验证标题到 ZIP 文件名的清理。
func TestZipFileName(t *testing.T) {
	cases := map[string]string{
		"":                 "wechat-article.zip",
		"正常标题":             "正常标题.zip",
		`a/b:c*d?"e"<f>|g`: "a_b_c_d__e__f__g.zip",
		"  .hidden.  ":     "hidden.zip",
	}
	for in, want := range cases {
		if got := zipFileName(in); got != want {
			t.Errorf("zipFileName(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestContentDisposition 验证中文文件名带 RFC 5987 编码。
func TestContentDisposition(t *testing.T) {
	ascii := contentDisposition("article.zip")
	if !strings.Contains(ascii, `filename="article.zip"`) {
		t.Errorf("ASCII 文件名兜底缺失: %s", ascii)
	}

	cn := contentDisposition("标题.zip")
	if !strings.Contains(cn, "filename*=UTF-8''") {
		t.Errorf("缺少 filename*: %s", cn)
	}
	if strings.Contains(cn, `filename="标题.zip"`) {
		t.Errorf("ASCII 兜底不应含中文: %s", cn)
	}
}

// newTestServer 构造一个使用临时输出目录的 Server。
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	logger := zerolog.Nop()
	cfg := config.New("test", &logger)
	cfg.OutputDir = root
	st, err := store.New(filepath.Join(root, ".wx_mp_sync_state.json"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	saver := sync.NewArticleSaver(cfg, st, &logger)
	return &Server{outputDir: root, saver: saver, logger: &logger}, root
}

// TestHandleDownload 验证注入按钮的下载接口能返回包含 HTML/Markdown 的 ZIP。
func TestHandleDownload(t *testing.T) {
	s, root := newTestServer(t)
	payload := `{
	  "url": "https://mp.weixin.qq.com/s/test",
	  "title": "测试文章",
	  "nickname": "测试号",
	  "cgiDataNew": {
	    "title": "测试文章",
	    "nick_name": "测试号",
	    "content_noencode": "<p>hello wechat</p>",
	    "create_time": "1760000000"
	  }
	}`

	doDownload := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleDownload(rec, req)
		return rec
	}

	rec := doDownload()
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition 缺少中文文件名编码: %q", cd)
	}

	names := zipNames(t, rec.Body.Bytes())
	for _, want := range []string{"index.html", "metadata.json", "测试文章.md"} {
		if !names[want] {
			t.Errorf("ZIP 缺少 %s,实际: %v", want, names)
		}
	}

	// 文章应已归档到输出目录
	var found bool
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "index.html" {
			found = true
		}
		return nil
	})
	if !found {
		t.Errorf("输出目录 %s 下未找到归档的 index.html", root)
	}

	// 第二次点击应命中去重,仍能正常下载
	rec2 := doDownload()
	if rec2.Code != http.StatusOK || rec2.Header().Get("X-Article-Duplicate") != "1" {
		t.Errorf("重复下载: code=%d duplicate=%q body=%s", rec2.Code, rec2.Header().Get("X-Article-Duplicate"), rec2.Body.String())
	}
	if len(zipNames(t, rec2.Body.Bytes())) == 0 {
		t.Errorf("重复下载的 ZIP 为空")
	}
}

// TestHandleDownloadSaved 验证管理页按目录下载与路径校验。
func TestHandleDownloadSaved(t *testing.T) {
	s, root := newTestServer(t)
	article := filepath.Join(root, "测试号", "2026-10", "标题")
	if err := os.MkdirAll(filepath.Join(article, "images"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(article, "index.html"), []byte("<html></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(article, "images", "a.jpg"), []byte("img"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(article, "metadata.json"), []byte(`{"title":"标题"}`), 0644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/download/saved?dir="+url.QueryEscape("测试号/2026-10/标题"), nil)
	rec := httptest.NewRecorder()
	s.handleDownloadSaved(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	names := zipNames(t, rec.Body.Bytes())
	if !names["index.html"] || !names["images/a.jpg"] || !names["metadata.json"] {
		t.Errorf("ZIP 文件清单不完整: %v", names)
	}

	bad := httptest.NewRequest(http.MethodGet, "/api/download/saved?dir="+url.QueryEscape("../../etc"), nil)
	badRec := httptest.NewRecorder()
	s.handleDownloadSaved(badRec, bad)
	if badRec.Code != http.StatusNotFound {
		t.Errorf("路径穿越应返回 404,实际 %d", badRec.Code)
	}
}

// zipNames 返回 ZIP 中所有文件的名称集合。
func zipNames(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	return names
}
