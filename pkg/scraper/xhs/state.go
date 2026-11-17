package xhs

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var initialStateMarkers = []string{
	"window.__INITIAL_STATE__",
	"window.__INITIAL_SSR_STATE__",
	"window.__INITIAL_STATE",
}

// ParseNoteHTML 从笔记页 HTML 里解析 window.__INITIAL_STATE__,提取笔记正文。
func ParseNoteHTML(html, noteID string) (*Note, error) {
	raw, err := extractInitialState(html)
	if err != nil {
		return nil, err
	}

	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("解析小红书页面数据失败: %w", err)
	}

	noteMap, id := findNote(state, noteID)
	if noteMap == nil {
		return nil, fmt.Errorf("小红书页面里没有找到笔记数据(可能是登录墙或页面改版)")
	}
	return buildNote(noteMap, id)
}

// ParseCapturedNote 解析注入脚本从页面里提取的精简笔记对象。
// 兼容 {"note":{...}} 和直接传笔记对象两种格式。
func ParseCapturedNote(raw []byte, noteID string) (*Note, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("笔记数据格式不正确: %w", err)
	}
	noteMap := mapField(payload, "note")
	if noteMap == nil {
		noteMap = payload
	}
	if nested := noteFromEntry(noteMap); nested != nil {
		noteMap = nested
	}
	return buildNote(noteMap, noteID)
}

// buildNote 把笔记 map 转成统一结构。
func buildNote(noteMap map[string]any, id string) (*Note, error) {
	note := &Note{
		ID:       NormalizeNoteID(firstNonEmpty(stringField(noteMap, "noteId"), stringField(noteMap, "noteID"), stringField(noteMap, "id"), id)),
		Title:    cleanText(stringField(noteMap, "title")),
		Desc:     cleanText(firstNonEmpty(stringField(noteMap, "desc"), stringField(noteMap, "content"))),
		Type:     strings.ToLower(strings.TrimSpace(stringField(noteMap, "type"))),
		VideoURL: extractVideoURL(noteMap),
	}

	author := mapField(noteMap, "user")
	if author == nil {
		author = mapField(noteMap, "author")
	}
	if author != nil {
		note.Author = cleanText(firstNonEmpty(
			stringField(author, "nickname"),
			stringField(author, "nickName"),
			stringField(author, "name"),
		))
		note.AuthorID = stringField(author, "userId")
	}

	note.Images = extractImages(noteMap["imageList"])
	if len(note.Images) == 0 {
		note.Images = extractImages(noteMap["images"])
	}
	note.Tags = extractTags(noteMap["tagList"])
	if len(note.Tags) == 0 {
		note.Tags = extractTags(noteMap["tags"])
	}
	note.PublishedAt = extractPublishedAt(noteMap)

	if note.Title == "" && note.Desc == "" && len(note.Images) == 0 {
		return nil, fmt.Errorf("小红书笔记没有正文或图片")
	}
	return note, nil
}

// extractInitialState 从 HTML 里截出 __INITIAL_STATE__ 的 JSON 文本,
// 并把 JS 特有的 undefined / NaN / Infinity 转成 null。
func extractInitialState(html string) ([]byte, error) {
	for _, marker := range initialStateMarkers {
		idx := strings.Index(html, marker)
		if idx < 0 {
			continue
		}
		rest := html[idx+len(marker):]
		eq := strings.Index(rest, "=")
		if eq < 0 {
			continue
		}
		rest = rest[eq+1:]
		end := strings.Index(rest, "</script>")
		if end < 0 {
			end = len(rest)
		}
		raw := strings.TrimSpace(rest[:end])
		raw = strings.TrimSuffix(raw, ";")
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		sanitized := sanitizeJSJSON(raw)
		if json.Valid([]byte(sanitized)) {
			return []byte(sanitized), nil
		}
	}
	return nil, fmt.Errorf("小红书页面里没有找到 __INITIAL_STATE__ 数据(可能需要登录或页面已改版)")
}

// sanitizeJSJSON 把 JSON 字符串外面的 undefined / NaN / Infinity 替换成 null。
func sanitizeJSJSON(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))

	inString := false
	escaped := false
	for i := 0; i < len(raw); {
		c := raw[i]
		if inString {
			b.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			i++
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			i++
			continue
		}
		if hasTokenAt(raw, i, "undefined") {
			b.WriteString("null")
			i += len("undefined")
			continue
		}
		if hasTokenAt(raw, i, "NaN") {
			b.WriteString("null")
			i += len("NaN")
			continue
		}
		if hasTokenAt(raw, i, "Infinity") {
			b.WriteString("null")
			i += len("Infinity")
			continue
		}
		if hasTokenAt(raw, i, "-Infinity") {
			b.WriteString("null")
			i += len("-Infinity")
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func hasTokenAt(raw string, index int, token string) bool {
	if index+len(token) > len(raw) {
		return false
	}
	if raw[index:index+len(token)] != token {
		return false
	}
	if index > 0 {
		prev := raw[index-1]
		if isIdentChar(prev) {
			return false
		}
	}
	if index+len(token) < len(raw) {
		next := raw[index+len(token)]
		if isIdentChar(next) {
			return false
		}
	}
	return true
}

func isIdentChar(c byte) bool {
	return c == '$' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// findNote 在状态数据里定位笔记详情,返回笔记 map 和笔记 ID。
func findNote(state map[string]any, noteID string) (map[string]any, string) {
	noteSection := mapField(state, "note")
	if noteSection != nil {
		currentID := firstNonEmpty(stringField(noteSection, "currentNoteId"), noteID)
		if detailMap := mapField(noteSection, "noteDetailMap"); detailMap != nil {
			if currentID != "" {
				if entry := mapField(detailMap, currentID); entry != nil {
					if note := noteFromEntry(entry); note != nil {
						return note, firstNonEmpty(stringField(note, "noteId"), currentID)
					}
				}
			}
			for key, value := range detailMap {
				entry, ok := value.(map[string]any)
				if !ok {
					continue
				}
				if note := noteFromEntry(entry); note != nil {
					return note, firstNonEmpty(stringField(note, "noteId"), key)
				}
			}
		}
		if note := noteFromEntry(noteSection); note != nil {
			return note, firstNonEmpty(stringField(note, "noteId"), currentID)
		}
	}

	if noteData := mapField(state, "noteData"); noteData != nil {
		if data := mapField(noteData, "data"); data != nil {
			if note := mapField(data, "noteData"); note != nil {
				return note, firstNonEmpty(stringField(note, "noteId"), noteID)
			}
			if note := mapField(data, "note"); note != nil {
				return note, firstNonEmpty(stringField(note, "noteId"), noteID)
			}
		}
		if note := mapField(noteData, "noteData"); note != nil {
			return note, firstNonEmpty(stringField(note, "noteId"), noteID)
		}
	}

	return nil, ""
}

// noteFromEntry 兼容 {note:{...}} 和直接是笔记对象的两种结构。
func noteFromEntry(entry map[string]any) map[string]any {
	if entry == nil {
		return nil
	}
	if note := mapField(entry, "note"); note != nil {
		return note
	}
	if _, ok := entry["imageList"]; ok {
		return entry
	}
	if _, ok := entry["desc"]; ok {
		return entry
	}
	if _, ok := entry["title"]; ok {
		return entry
	}
	return nil
}

// extractImages 从 imageList 里取出图片地址,兼容 urlDefault / urlPre / url / infoList。
func extractImages(raw any) []Image {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}

	images := make([]Image, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		switch value := item.(type) {
		case string:
			addImage(&images, seen, value, 0, 0)
		case map[string]any:
			urlValue := firstNonEmpty(
				stringField(value, "urlDefault"),
				stringField(value, "urlPre"),
				stringField(value, "url"),
				stringField(value, "masterUrl"),
			)
			if urlValue == "" {
				urlValue = firstInfoListURL(value["infoList"])
			}
			addImage(&images, seen, urlValue, intField(value, "width"), intField(value, "height"))
		}
	}
	return images
}

func firstInfoListURL(raw any) string {
	list, ok := raw.([]any)
	if !ok {
		return ""
	}
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		scene := stringField(entry, "imageScene")
		urlValue := firstNonEmpty(stringField(entry, "url"), stringField(entry, "urlDefault"))
		if urlValue == "" {
			continue
		}
		if scene == "" || strings.EqualFold(scene, "WB_DFT") {
			return urlValue
		}
	}
	return ""
}

func addImage(images *[]Image, seen map[string]bool, rawURL string, width, height int) {
	urlValue := normalizeImageURL(rawURL)
	if urlValue == "" || seen[urlValue] {
		return
	}
	seen[urlValue] = true
	*images = append(*images, Image{URL: urlValue, Width: width, Height: height})
}

func extractTags(raw any) []string {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	tags := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		var tag string
		switch value := item.(type) {
		case string:
			tag = value
		case map[string]any:
			tag = firstNonEmpty(stringField(value, "name"), stringField(value, "title"))
		}
		tag = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tag), "#"))
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	return tags
}

func extractPublishedAt(note map[string]any) string {
	for _, key := range []string{"time", "publishTime", "lastUpdateTime", "createTime"} {
		ms := int64Field(note, key)
		if ms <= 0 {
			continue
		}
		if ms > 1_000_000_000_000 {
			return time.UnixMilli(ms).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
		}
		if ms > 1_000_000_000 {
			return time.Unix(ms, 0).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
		}
	}
	return ""
}

func extractVideoURL(note map[string]any) string {
	video := mapField(note, "video")
	if video == nil {
		return ""
	}
	media := mapField(video, "media")
	if media == nil {
		media = video
	}
	stream := mapField(media, "stream")
	if stream == nil {
		return ""
	}
	for _, codec := range []string{"h264", "h265", "av1"} {
		list, ok := stream[codec].([]any)
		if !ok {
			continue
		}
		for _, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if urlValue := firstNonEmpty(stringField(entry, "masterUrl"), stringField(entry, "url")); urlValue != "" {
				return normalizeImageURL(urlValue)
			}
		}
	}
	return ""
}

func mapField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	value, _ := m[key].(map[string]any)
	return value
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch value := m[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	}
	return ""
}

func intField(m map[string]any, key string) int {
	return int(int64Field(m, key))
}

func int64Field(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch value := m[key].(type) {
	case float64:
		return int64(value)
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		return parsed
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func cleanText(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.TrimSpace(raw)
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
