// Package sync 实现公众号文章的同步引擎:汇总来源 → 并发抓取 → 本地归档 → 更新状态。
package sync

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"wx-mp-sync/internal/config"
	"wx-mp-sync/internal/store"
	"wx-mp-sync/pkg/scraper/wxmp"
)

// Failure 记录一篇失败的文章及其原因。
type Failure struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// Result 是单次同步的统计结果。
type Result struct {
	Total    int       `json:"total"`
	Synced   int       `json:"synced"`
	Skipped  int       `json:"skipped"`
	Failed   int       `json:"failed"`
	Paths    []string  `json:"paths"`
	Failures []Failure `json:"failures"`
}

type job struct {
	url         string
	accountName string
}

// Syncer 是同步引擎。
type Syncer struct {
	cfg     *config.Config
	st      *store.Store
	dl      *wxmp.OfficialAccountDownload
	acct    *wxmp.AccountClient
	logger  *zerolog.Logger
	timeout time.Duration
}

// NewSyncer 创建同步引擎。
func NewSyncer(cfg *config.Config, logger *zerolog.Logger) *Syncer {
	dl := &wxmp.OfficialAccountDownload{}
	if cfg.Concurrency > 0 {
		dl.OnProgress = func(n int64) {}
	}
	return &Syncer{
		cfg:     cfg,
		dl:      dl,
		acct:    wxmp.NewAccountClient(logger),
		logger:  logger,
		timeout: time.Duration(cfg.TimeoutSec) * time.Second,
	}
}

// Run 执行一次同步,返回统计结果。
func (s *Syncer) Run(ctx context.Context, extraURLs []string) (*Result, error) {
	if err := s.cfg.Normalize(); err != nil {
		return nil, err
	}

	statePath := filepath.Join(s.cfg.OutputDir, ".wx_mp_sync_state.json")
	st, err := store.New(statePath)
	if err != nil {
		return nil, fmt.Errorf("打开同步状态失败: %w", err)
	}
	s.st = st

	jobs, err := s.collectURLs(extraURLs)
	if err != nil {
		return nil, err
	}
	s.logger.Info().Int("urls", len(jobs)).Msg("收集到待处理文章")

	return s.processJobs(ctx, jobs)
}

// collectURLs 汇总直接 URL 与账号列表拉取到的 URL。
func (s *Syncer) collectURLs(extra []string) ([]job, error) {
	seen := map[string]bool{}
	var jobs []job

	add := func(u, accountName string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		jobs = append(jobs, job{url: u, accountName: accountName})
	}

	for _, u := range s.cfg.Articles {
		add(u, "")
	}
	for _, u := range extra {
		add(u, "")
	}

	for _, acctCfg := range s.cfg.Accounts {
		name := acctCfg.Name
		if name == "" {
			name = acctCfg.Biz
		}
		acct := &wxmp.OfficialAccount{
			Name:        name,
			Biz:         acctCfg.Biz,
			Uin:         acctCfg.Uin,
			Key:         acctCfg.Key,
			PassTicket:  acctCfg.PassTicket,
			AppmsgToken: acctCfg.AppmsgToken,
			AuthorID:    acctCfg.AuthorID,
			MaxArticles: acctCfg.MaxArticles,
		}
		if acct.Biz == "" || acct.Key == "" {
			s.logger.Warn().Str("account", name).Msg("账号缺少 biz/key,跳过")
			continue
		}
		urls, err := s.acct.FetchAllMsgURLs(acct)
		if err != nil {
			s.logger.Warn().Str("account", name).Err(err).Msg("拉取账号文章列表失败")
			continue
		}
		s.logger.Info().Str("account", name).Int("found", len(urls)).Msg("账号列表拉取完成")
		for _, u := range urls {
			add(u, name)
		}
	}

	return jobs, nil
}

// processJobs 以固定并发数处理所有文章。
func (s *Syncer) processJobs(ctx context.Context, jobs []job) (*Result, error) {
	result := &Result{Total: len(jobs)}

	var mu sync.Mutex
	report := func(delta func(*Result)) {
		mu.Lock()
		delta(result)
		mu.Unlock()
	}

	workers := s.cfg.Concurrency
	if workers <= 0 {
		workers = 1
	}

	jobCh := make(chan job)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				if err := ctx.Err(); err != nil {
					report(func(r *Result) { r.Failed++ })
					continue
				}
				s.syncOne(ctx, j, report)
			}
		}()
	}

feed:
	for _, j := range jobs {
		select {
		case jobCh <- j:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobCh)
	wg.Wait()

	return result, nil
}

// syncOne 同步单篇文章。
func (s *Syncer) syncOne(ctx context.Context, j job, report func(func(*Result))) {
	urlKey := urlExternalID(j.url)
	if s.st.Has(urlKey) {
		s.logger.Debug().Str("url", j.url).Msg("已同步,跳过")
		report(func(r *Result) { r.Skipped++ })
		return
	}

	article, err := s.dl.FetchArticle(j.url)
	if err != nil {
		s.logger.Error().Str("url", j.url).Err(err).Msg("抓取文章失败")
		report(func(r *Result) {
			r.Failed++
			r.Failures = append(r.Failures, Failure{URL: j.url, Error: err.Error()})
		})
		return
	}

	externalID := articleExternalID(article, j.url)
	if s.st.Has(externalID) {
		// 已用规范 id 同步过,补充 url 索引后跳过
		_ = s.st.Put(urlKey, &store.Record{ExternalID: externalID, URL: j.url, Title: article.Title})
		s.logger.Debug().Str("id", externalID).Msg("文章已存在,跳过")
		report(func(r *Result) { r.Skipped++ })
		return
	}

	dir, err := articleDirFor(s.cfg, j.accountName, article)
	if err != nil {
		report(func(r *Result) {
			r.Failed++
			r.Failures = append(r.Failures, Failure{URL: j.url, Error: err.Error()})
		})
		return
	}

	htmlName, mdName, err := s.dl.SaveArticle(article, dir)
	if err != nil {
		s.logger.Error().Str("url", j.url).Err(err).Msg("归档文章失败")
		report(func(r *Result) {
			r.Failed++
			r.Failures = append(r.Failures, Failure{URL: j.url, Error: err.Error()})
		})
		return
	}

	if err := saveCover(s.dl, article, dir); err != nil {
		s.logger.Debug().Str("url", j.url).Err(err).Msg("封面下载失败(忽略)")
	}
	if err := saveMetadata(article, j.url, externalID, dir, htmlName, mdName); err != nil {
		s.logger.Debug().Str("url", j.url).Err(err).Msg("元数据写入失败(忽略)")
	}

	now := time.Now().Unix()
	rec := &store.Record{
		ExternalID: externalID,
		URL:        j.url,
		Title:      article.Title,
		Dir:        dir,
		HTML:       htmlName,
		Markdown:   mdName,
		SyncedAt:   now,
	}
	if err := s.st.Put(externalID, rec); err != nil {
		s.logger.Error().Err(err).Msg("写入同步状态失败")
	}
	_ = s.st.Put(urlKey, rec)

	s.logger.Info().Str("title", article.Title).Str("dir", dir).Msg("文章同步完成")
	report(func(r *Result) {
		r.Synced++
		r.Paths = append(r.Paths, dir)
	})
}

// publishTime 从文章数据提取发布时间(时区本地)。
func publishTime(article *wxmp.WechatOfficialArticle) time.Time {
	if article == nil || article.PageJSON == nil {
		return time.Time{}
	}
	data := article.PageJSON
	if data.OriCreateTime > 0 {
		return time.Unix(int64(data.OriCreateTime), 0)
	}
	if data.CreateTime != "" {
		if t, err := time.Parse("2006-01-02 15:04", strings.TrimSpace(data.CreateTime)); err == nil {
			return t
		}
	}
	return time.Time{}
}

// articleExternalID 生成文章的唯一标识:优先 biz_mid_idx,失败回退到 URL。
func articleExternalID(article *wxmp.WechatOfficialArticle, rawURL string) string {
	if article != nil && article.PageJSON != nil {
		data := article.PageJSON
		if strings.TrimSpace(data.BizUin) != "" && data.Mid > 0 && data.Idx > 0 {
			return fmt.Sprintf("%s_%d_%d", strings.TrimSpace(data.BizUin), data.Mid, data.Idx)
		}
	}
	return urlExternalID(rawURL)
}

// urlExternalID 从 URL 派生出用于去重的标识。
func urlExternalID(rawURL string) string {
	if id := wxmp.ExtractArticleID(rawURL); id != "" {
		return "url_" + id
	}
	sum := md5.Sum([]byte(rawURL))
	return "url_" + hex.EncodeToString(sum[:])
}

// sanitizeDir 把目录名清洗为合法路径。
func sanitizeDir(s string) string {
	re := regexp.MustCompile(`[\\/:*?"<>|\r\n\t]`)
	s = re.ReplaceAllString(s, "_")
	s = strings.TrimSpace(s)
	if s == "" || s == "." || s == ".." {
		return "untitled"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// fileExists 判断路径是否存在。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
