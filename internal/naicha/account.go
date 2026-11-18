package naicha

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// sessionCookieName 是奶茶圈登录后的会话 Cookie 名。
const sessionCookieName = "bbs_session"

// maxPublishContentUnits 与新论坛的正文长度上限保持一致。
const maxPublishContentUnits = 50000

// PublishPost 是走新版论坛接口(管理员登录 + /api/posts/create)的帖子。
type PublishPost struct {
	Title     string
	Content   string
	Board     string // boardSlug;空时用配置的 Board
	Tags      []string
	Images    []PostImage // Data 优先;URL 非空时由论坛自行抓取外链
	Source    string
	DedupeKey string // 去重键,例如 xhs:<noteId>;为空时不去重
}

// PublishResult 是发布小红书等外部内容后的结果。
type PublishResult struct {
	OK           bool
	Duplicate    bool
	ID           string
	URL          string
	Title        string
	Images       int
	FailedImages int
	Pending      bool
	Message      string
}

// Publish 用管理员会话发布一条帖子:先上传图片,再调用 /api/posts/create。
func (c *Client) Publish(ctx context.Context, post *PublishPost, force bool) PublishResult {
	if c == nil {
		return PublishResult{Message: "没有启用奶茶圈同步"}
	}
	if post == nil {
		return PublishResult{Message: "帖子内容为空"}
	}

	key := strings.TrimSpace(post.DedupeKey)
	if key != "" && !force {
		if rec, ok := c.state.Get(key); ok {
			return PublishResult{
				OK:        true,
				Duplicate: true,
				ID:        rec.PostID,
				URL:       rec.URL,
				Title:     rec.Title,
				Message:   "这篇内容已经同步过了",
			}
		}
	}

	// 配置了 BOT_API_SECRET 时优先走机器人接口,不需要管理员登录。
	if c.BotReady() {
		return c.publishBot(ctx, post, key)
	}

	if _, err := c.ensureSession(ctx); err != nil {
		return PublishResult{Message: err.Error()}
	}

	imagePaths := make([]string, 0, len(post.Images))
	failedUploads := 0
	for i, img := range post.Images {
		urlPath, err := c.uploadImage(ctx, img)
		if err != nil {
			failedUploads++
			c.log.Warn().Err(err).Int("index", i+1).Str("file", img.Filename).Msg("奶茶圈图片上传失败,已跳过")
			continue
		}
		imagePaths = append(imagePaths, urlPath)
	}
	if len(post.Images) > 0 && len(imagePaths) == 0 {
		return PublishResult{Message: "图片全部上传失败", FailedImages: failedUploads}
	}

	title := normalizeTitle(post.Title)
	// 新论坛默认要求标题至少 5 个字;XHS 笔记标题可能只有 2-3 个字。
	if textLen(title) < 5 {
		title += " 笔记"
	}
	content := appendImageMarkdown(post.Content, imagePaths)
	board := strings.TrimSpace(post.Board)
	if board == "" {
		board = c.cfg.Board
	}
	if board == "" {
		board = "new-releases"
	}
	tags := post.Tags
	if len(tags) == 0 {
		tags = c.cfg.Tags
	}

	payload := map[string]any{
		"title":       title,
		"content":     content,
		"boardSlug":   board,
		"postType":    "NORMAL",
		"isAnonymous": false,
	}
	if len(tags) > 0 {
		payload["manualTags"] = tags
	}
	if len(imagePaths) > 0 {
		payload["coverPath"] = imagePaths[0]
	}

	var created createPostResponse
	status, body, err := c.postJSONAuthed(ctx, "/api/posts/create", payload, &created)
	if err != nil {
		c.log.Error().Err(err).Str("title", title).Msg("奶茶圈发帖失败")
		return PublishResult{Message: err.Error()}
	}
	if status < 200 || status >= 300 {
		return PublishResult{Message: apiErrorMessage(status, body)}
	}

	postID := strings.TrimSpace(created.Data.ID)
	if postID == "" {
		return PublishResult{Message: "奶茶圈返回的帖子 ID 为空"}
	}
	postURL := c.postURL(postID)
	pending := !strings.EqualFold(created.Data.Status, "NORMAL")

	rec := Record{
		PostID:   postID,
		URL:      postURL,
		Title:    title,
		SyncedAt: time.Now().Format(time.RFC3339),
	}
	if key != "" {
		if err := c.state.Put(key, rec); err != nil {
			c.log.Warn().Err(err).Str("key", key).Msg("写入奶茶圈同步状态失败")
		}
	}

	c.log.Info().
		Str("title", title).
		Str("id", postID).
		Str("url", postURL).
		Int("images", len(imagePaths)).
		Bool("pending", pending).
		Msg("奶茶圈发帖成功")

	return PublishResult{
		OK:           true,
		ID:           postID,
		URL:          postURL,
		Title:        title,
		Images:       len(imagePaths),
		FailedImages: failedUploads,
		Pending:      pending,
		Message:      created.Message,
	}
}

// SessionReady 表示是否配置了管理员登录或可用的会话 Cookie。
func (c *Client) SessionReady() bool {
	if c == nil {
		return false
	}
	return c.cfg.Cookie != "" || (c.cfg.Username != "" && c.cfg.Password != "")
}

// Board 返回配置的发帖分类。
func (c *Client) Board() string {
	if c == nil {
		return ""
	}
	return c.cfg.Board
}

// normalizeSessionCookie 把配置里的 Cookie 统一成请求头形式。
func normalizeSessionCookie(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "=") {
		return raw
	}
	return sessionCookieName + "=" + raw
}

// ensureSession 返回可用的 Cookie 请求头,必要时先登录。
func (c *Client) ensureSession(ctx context.Context) (string, error) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()

	if c.session != "" {
		return c.session, nil
	}
	if c.cfg.Cookie != "" {
		c.session = c.cfg.Cookie
		return c.session, nil
	}
	if err := c.loginLocked(ctx); err != nil {
		return "", err
	}
	return c.session, nil
}

// resetSession 在会话失效时清空缓存,下一次请求重新登录。
func (c *Client) resetSession() {
	c.sessionMu.Lock()
	c.session = ""
	c.sessionMu.Unlock()
}

// loginLocked 用用户名/密码登录奶茶圈;开启 PoW 验证码时自动求解。
func (c *Client) loginLocked(ctx context.Context) error {
	if c.cfg.Username == "" || c.cfg.Password == "" {
		return fmt.Errorf("未配置奶茶圈管理员登录:请在 config.yaml 的 naicha_sync 里填写 username 和 password,或填写 cookie")
	}

	payload := map[string]any{
		"login":    c.cfg.Username,
		"password": c.cfg.Password,
	}
	// 先主动取一次 PoW 挑战:站点开启 PoW 时直接带上,避免第一次登录被
	// 验证码拒绝后触发登录接口的 2 秒写保护冷却。未开启 PoW 时接口返回 503,
	// 忽略即可。
	if token, nonce, powErr := c.solvePow(ctx); powErr == nil {
		payload["captchaToken"] = token
		payload["powNonce"] = nonce
	}
	status, body, resp, err := c.postJSON(ctx, "/api/auth/login", payload, "")
	if err != nil {
		return fmt.Errorf("登录奶茶圈失败: %w", err)
	}

	message := apiMessage(body)
	if status == http.StatusBadRequest && isPowRequired(message) {
		// 验证码接口本身有 1 秒写保护冷却,等一下再取。
		if err := sleepContext(ctx, 1200*time.Millisecond); err != nil {
			return err
		}
		token, nonce, powErr := c.solvePow(ctx)
		if powErr != nil {
			return fmt.Errorf("奶茶圈登录需要工作量证明: %w", powErr)
		}
		payload["captchaToken"] = token
		payload["powNonce"] = nonce
		status, body, resp, err = c.postJSON(ctx, "/api/auth/login", payload, "")
		if err != nil {
			return fmt.Errorf("登录奶茶圈失败: %w", err)
		}
		message = apiMessage(body)
	}

	if status < 200 || status >= 300 {
		return fmt.Errorf("奶茶圈登录失败: %s", loginErrorMessage(status, message))
	}

	cookie := findSessionCookie(resp)
	if cookie == "" {
		return fmt.Errorf("奶茶圈登录成功但没有返回 %s Cookie", sessionCookieName)
	}
	c.session = sessionCookieName + "=" + cookie
	c.log.Info().Str("user", c.cfg.Username).Msg("奶茶圈管理员登录成功")
	return nil
}

// solvePow 获取并求解 /api/auth/pow 的工作量证明。
func (c *Client) solvePow(ctx context.Context) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIBase+"/api/auth/pow?scope=login", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "wx-mp-sync/naicha")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var parsed struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			CaptchaToken string `json:"captchaToken"`
			Difficulty   int    `json:"difficulty"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, fmt.Errorf("解析验证码响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || parsed.Data.CaptchaToken == "" {
		msg := parsed.Message
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return "", 0, fmt.Errorf("获取验证码失败(%d): %s", resp.StatusCode, msg)
	}

	parts := strings.Split(parsed.Data.CaptchaToken, "-")
	if len(parts) < 1 || parts[0] == "" {
		return "", 0, fmt.Errorf("验证码格式不正确")
	}
	randomData := parts[0]
	difficulty := parsed.Data.Difficulty
	if difficulty < 1 {
		difficulty = 1
	}
	if difficulty > 6 {
		difficulty = 6
	}
	for nonce := int64(0); nonce < 50_000_000; nonce++ {
		if nonce%100_000 == 0 {
			select {
			case <-ctx.Done():
				return "", 0, ctx.Err()
			default:
			}
		}
		sum := sha256.Sum256([]byte(randomData + strconv.FormatInt(nonce, 10)))
		if hasZeroHexPrefix(sum, difficulty) {
			return parsed.Data.CaptchaToken, nonce, nil
		}
	}
	return "", 0, fmt.Errorf("计算工作量证明超时")
}

// hasZeroHexPrefix 判断 sha256 结果的十六进制表示是否以 difficulty 个 0 开头。
// 直接按半字节判断,避免每次迭代都 hex 编码。
func hasZeroHexPrefix(sum [32]byte, difficulty int) bool {
	fullBytes := difficulty / 2
	for i := 0; i < fullBytes; i++ {
		if sum[i] != 0 {
			return false
		}
	}
	if difficulty%2 == 1 {
		return sum[fullBytes]>>4 == 0
	}
	return true
}

// uploadImage 把图片上传到论坛图床并返回可访问的 urlPath。
func (c *Client) uploadImage(ctx context.Context, img PostImage) (string, error) {
	data := img.Data
	if len(data) == 0 {
		if img.URL == "" {
			return "", fmt.Errorf("图片没有数据或地址")
		}
		return "", fmt.Errorf("暂不支持由论坛抓取外链图片: %s", img.URL)
	}
	filename := sanitizeImageFilename(img.Filename, data)

	sessionRetries := 0
	rateRetries := 0
	for attempt := 0; attempt < 5; attempt++ {
		cookie, err := c.ensureSession(ctx)
		if err != nil {
			return "", err
		}

		buf := &bytes.Buffer{}
		mw := multipart.NewWriter(buf)
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeMultipartQuotes(filename)))
		header.Set("Content-Type", imageMIME(data))
		part, err := mw.CreatePart(header)
		if err != nil {
			return "", err
		}
		if _, err := part.Write(data); err != nil {
			return "", err
		}
		if err := mw.WriteField("folder", "posts"); err != nil {
			return "", err
		}
		if err := mw.Close(); err != nil {
			return "", err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIBase+"/api/upload", buf)
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Cookie", cookie)
		req.Header.Set("User-Agent", "wx-mp-sync/naicha")

		resp, err := c.http.Do(req)
		if err != nil {
			return "", fmt.Errorf("上传图片失败: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized {
			if sessionRetries == 0 {
				sessionRetries++
				c.resetSession()
				continue
			}
			return "", fmt.Errorf("奶茶圈登录已失效,请检查 naicha_sync 的用户名密码或 cookie")
		}
		// 论坛默认两次上传至少间隔 2 秒;图片较多时命中 429,等一会再试。
		if resp.StatusCode == http.StatusTooManyRequests && rateRetries < 2 {
			rateRetries++
			if err := sleepContext(ctx, 2200*time.Millisecond); err != nil {
				return "", err
			}
			continue
		}

		var parsed struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				URLPath string `json:"urlPath"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &parsed)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || parsed.Code != 0 || parsed.Data.URLPath == "" {
			return "", fmt.Errorf("上传图片失败(%d): %s", resp.StatusCode, firstNonEmpty(parsed.Message, strings.TrimSpace(string(body)), resp.Status))
		}
		return parsed.Data.URLPath, nil
	}
	return "", fmt.Errorf("上传图片失败: 登录状态反复失效")
}

type createPostResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		URL    string `json:"url"`
	} `json:"data"`
}

// postJSONAuthed 发送带会话 Cookie 的 JSON 请求;401 时重新登录并重试一次。
func (c *Client) postJSONAuthed(ctx context.Context, path string, payload any, out any) (int, []byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		cookie, err := c.ensureSession(ctx)
		if err != nil {
			return 0, nil, err
		}
		status, body, _, err := c.postJSON(ctx, path, payload, cookie)
		if err != nil {
			return 0, nil, err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.resetSession()
			continue
		}
		if out != nil && len(body) > 0 {
			_ = json.Unmarshal(body, out)
		}
		return status, body, nil
	}
	return http.StatusUnauthorized, nil, fmt.Errorf("登录状态反复失效")
}

// postJSON 发送 JSON 请求并返回状态码、响应体和响应头。
func (c *Client) postJSON(ctx context.Context, path string, payload any, cookie string) (int, []byte, *http.Response, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIBase+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wx-mp-sync/naicha")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, resp, nil
}

// findSessionCookie 从登录响应里取出会话 Cookie 的值。
func findSessionCookie(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			return cookie.Value
		}
	}
	return ""
}

func isPowRequired(message string) bool {
	return strings.Contains(message, "工作量证明") || strings.Contains(strings.ToLower(message), "pow")
}

func loginErrorMessage(status int, message string) string {
	if status == http.StatusTooManyRequests {
		return "登录过于频繁,请稍后再试"
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if strings.Contains(message, "验证码") || strings.Contains(message, "Turnstile") {
		return message + "。请在 naicha_sync.cookie 里粘贴浏览器登录后的 bbs_session,或关闭登录验证码"
	}
	return message
}

func apiMessage(body []byte) string {
	var parsed struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	if parsed.Message != "" {
		return parsed.Message
	}
	return parsed.Error
}

func apiErrorMessage(status int, body []byte) string {
	message := apiMessage(body)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if status == http.StatusUnauthorized {
		return "奶茶圈登录已失效,请检查 naicha_sync 的用户名密码或 cookie"
	}
	return fmt.Sprintf("奶茶圈发布失败(%d): %s", status, message)
}

func (c *Client) postURL(id string) string {
	base := strings.TrimRight(strings.TrimSpace(c.cfg.APIBase), "/")
	if base == "" {
		return id
	}
	return base + "/post/" + url.PathEscape(id)
}

// appendImageMarkdown 把上传后的图片以 Markdown 形式追加到正文末尾。
func appendImageMarkdown(content string, imagePaths []string) string {
	content = strings.TrimSpace(content)
	if len(imagePaths) == 0 {
		if content == "" {
			content = "（内容为空）"
		}
		content = truncateText(content, maxPublishContentUnits)
		if textLen(content) < minContentUnits {
			content += "\n\n（全文完）"
		}
		return content
	}

	var lines []string
	for i, imagePath := range imagePaths {
		lines = append(lines, fmt.Sprintf("![图%d](%s)", i+1, imagePath))
	}
	images := strings.Join(lines, "\n\n")
	if content == "" {
		content = images
	} else {
		content += "\n\n" + images
	}
	if textLen(content) > maxPublishContentUnits {
		content = truncateText(content, maxPublishContentUnits)
	}
	if textLen(content) < minContentUnits {
		content += "\n\n（全文完）"
	}
	return content
}

// appendImageLinks 把原图地址以 Markdown 图片形式追加到正文末尾。
// 机器人接口只能发文本,不能上传图片,因此直接引用小红书 CDN 的原图链接。
func appendImageLinks(content string, images []PostImage) string {
	lines := make([]string, 0, len(images))
	for i, img := range images {
		u := strings.TrimSpace(img.URL)
		if u == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("![图%d](%s)", i+1, u))
	}
	content = strings.TrimSpace(content)
	if len(lines) == 0 {
		if content == "" {
			content = "（内容为空）"
		}
		content = truncateText(content, maxPublishContentUnits)
		if textLen(content) < minContentUnits {
			content += "\n\n（全文完）"
		}
		return content
	}

	links := strings.Join(lines, "\n\n")
	if content == "" {
		content = links
	} else {
		content += "\n\n" + links
	}
	if textLen(content) > maxPublishContentUnits {
		content = truncateText(content, maxPublishContentUnits)
	}
	return content
}

// sanitizeImageFilename 生成论坛可接受的图片文件名(扩展名必须匹配内容)。
func sanitizeImageFilename(name string, data []byte) string {
	ext := extensionForMIME(imageMIME(data))
	base := strings.TrimSpace(name)
	if base == "" {
		base = "image"
	}
	base = strings.TrimSuffix(base, path.Ext(base))
	base = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', '\r', '\n', '\t', '"':
			return '_'
		}
		if r < 32 {
			return -1
		}
		return r
	}, base)
	base = strings.TrimSpace(base)
	if base == "" || base == "image" {
		base = "image-" + shortHash(data)
	}
	if len([]rune(base)) > 80 {
		base = string([]rune(base)[:80])
	}
	return base + ext
}

func imageMIME(data []byte) string {
	if len(data) == 0 {
		return "application/octet-stream"
	}
	mime := http.DetectContentType(data)
	if strings.HasPrefix(mime, "image/") {
		return mime
	}
	return "application/octet-stream"
}

func extensionForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/avif":
		return ".avif"
	default:
		return ".jpg"
	}
}

func escapeMultipartQuotes(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}

// shortHash 用于给没有文件名的图片生成稳定文件名。
func shortHash(data []byte) string {
	h := fnv.New32a()
	_, _ = h.Write(data)
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
