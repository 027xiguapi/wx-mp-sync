package sync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"wx-mp-sync/internal/config"
	"wx-mp-sync/internal/store"
	"wx-mp-sync/pkg/scraper/wxmp"
)

// ArticleSaver 从注入页面的 cgiDataNew 或文章 URL 归档文章到本地。
type ArticleSaver struct {
	Cfg    *config.Config
	Store  *store.Store
	Logger *zerolog.Logger
	dl     *wxmp.OfficialAccountDownload
}

// NewArticleSaver 创建保存器。
func NewArticleSaver(cfg *config.Config, st *store.Store, logger *zerolog.Logger) *ArticleSaver {
	return &ArticleSaver{
		Cfg:    cfg,
		Store:  st,
		Logger: logger,
		dl:     &wxmp.OfficialAccountDownload{},
	}
}

// SaveRequest 是注入按钮 / 手动同步 POST 的载荷。
type SaveRequest struct {
	URL        string          `json:"url"`
	Title      string          `json:"title"`
	Nickname   string          `json:"nickname"`
	CgiDataNew json.RawMessage `json:"cgiDataNew"`
}

// SaveFromCgiData 从注入页面发来的文章数据归档;数据缺正文时回退按 URL 抓取。
func (s *ArticleSaver) SaveFromCgiData(rawURL string, raw json.RawMessage) (string, bool, *wxmp.WechatOfficialArticle, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return s.SaveFromURL(rawURL)
	}
	var data wxmp.CgiDataNew
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", false, nil, fmt.Errorf("解析 cgiDataNew 失败: %w", err)
	}
	article := wxmp.NewArticleFromCgiData(&data, wxmp.FormatArticlePublishTime(&data))
	if article == nil || strings.TrimSpace(article.Content) == "" {
		return s.SaveFromURL(rawURL)
	}
	return s.saveArticle(article, rawURL, strings.TrimSpace(data.NickName))
}

// SaveFromURL 按 URL 抓取并归档文章。
// 若传入的是搜狗微信的 /link 跳转链接,会先还原成真实的公众号文章链接。
func (s *ArticleSaver) SaveFromURL(rawURL string) (string, bool, *wxmp.WechatOfficialArticle, error) {
	if wxmp.IsSogouLink(rawURL) {
		resolved, err := wxmp.ResolveSogouLink(rawURL)
		if err != nil {
			return "", false, nil, fmt.Errorf("还原搜狗文章链接失败: %w", err)
		}
		if s.Logger != nil {
			s.Logger.Info().Str("from", rawURL).Str("to", resolved).Msg("已还原搜狗跳转链接")
		}
		rawURL = resolved
	}
	article, err := s.dl.FetchArticle(rawURL)
	if err != nil {
		return "", false, nil, err
	}
	return s.saveArticle(article, rawURL, "")
}

// saveArticle 去重并归档一篇文章,返回目录、是否重复、文章、错误。
func (s *ArticleSaver) saveArticle(article *wxmp.WechatOfficialArticle, rawURL, accountName string) (string, bool, *wxmp.WechatOfficialArticle, error) {
	if err := s.Cfg.Normalize(); err != nil {
		return "", false, article, err
	}

	externalID := articleExternalID(article, rawURL)
	if s.Store.Has(externalID) {
		if rec, ok := s.Store.Get(externalID); ok && rec.Dir != "" {
			// 目录可能被用户手动删除,存在时才复用,否则重新归档。
			if st, statErr := os.Stat(rec.Dir); statErr == nil && st.IsDir() {
				return rec.Dir, true, article, nil
			}
		}
	}

	dir, err := articleDirFor(s.Cfg, accountName, article)
	if err != nil {
		return "", false, article, err
	}
	htmlName, mdName, err := s.dl.SaveArticle(article, dir)
	if err != nil {
		return "", false, article, err
	}
	if err := saveCover(s.dl, article, dir); err != nil {
		s.Logger.Debug().Str("url", rawURL).Err(err).Msg("封面下载失败(忽略)")
	}
	if err := saveMetadata(article, rawURL, externalID, dir, htmlName, mdName); err != nil {
		s.Logger.Debug().Str("url", rawURL).Err(err).Msg("元数据写入失败(忽略)")
	}

	now := time.Now().Unix()
	if err := s.Store.Put(externalID, &store.Record{
		ExternalID: externalID,
		URL:        rawURL,
		Title:      article.Title,
		Dir:        dir,
		HTML:       htmlName,
		Markdown:   mdName,
		SyncedAt:   now,
	}); err != nil {
		s.Logger.Error().Err(err).Msg("写入同步状态失败")
	}
	if urlKey := urlExternalID(rawURL); urlKey != externalID {
		_ = s.Store.Put(urlKey, &store.Record{ExternalID: externalID, URL: rawURL, Title: article.Title})
	}

	s.Logger.Info().Str("title", article.Title).Str("dir", dir).Msg("文章保存成功")
	return dir, false, article, nil
}

// articleDirFor 计算文章的归档目录,重复标题自动加序号。
func articleDirFor(cfg *config.Config, accountName string, article *wxmp.WechatOfficialArticle) (string, error) {
	if accountName == "" {
		accountName = article.AuthorNickname
	}
	if accountName == "" {
		accountName = "wechat"
	}

	month := time.Now().Format("2006-01")
	if pt := publishTime(article); !pt.IsZero() {
		month = pt.Format("2006-01")
	}

	base := filepath.Join(cfg.OutputDir, sanitizeDir(accountName), month)
	if err := os.MkdirAll(base, 0755); err != nil {
		return "", err
	}

	title := sanitizeDir(article.Title)
	dir := filepath.Join(base, title)
	for i := 2; fileExists(dir); i++ {
		dir = filepath.Join(base, fmt.Sprintf("%s-%d", title, i))
	}
	return dir, nil
}

// saveCover 下载文章封面到归档目录根部。
func saveCover(dl *wxmp.OfficialAccountDownload, article *wxmp.WechatOfficialArticle, dir string) error {
	if article == nil || article.PageJSON == nil {
		return fmt.Errorf("无封面数据")
	}
	coverURL := strings.TrimSpace(article.PageJSON.CdnURL)
	if coverURL == "" {
		return fmt.Errorf("封面为空")
	}
	_, err := dl.DownloadFile(coverURL, dir)
	return err
}

// saveMetadata 写入文章的元数据 JSON。
func saveMetadata(article *wxmp.WechatOfficialArticle, rawURL, externalID, dir, htmlName, mdName string) error {
	meta := map[string]any{
		"external_id":  externalID,
		"title":        article.Title,
		"author":       article.AuthorNickname,
		"creator":      article.Creator,
		"publish_time": article.PublishTimeStr,
		"url":          rawURL,
		"html":         htmlName,
		"markdown":     mdName,
		"synced_at":    time.Now().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0644)
}
