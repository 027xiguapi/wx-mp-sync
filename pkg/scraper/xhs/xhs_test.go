package xhs

import (
	"encoding/json"
	"strings"
	"testing"
)

const sampleNoteID = "66f1a2b3c4d5e6f708192a3b"

func sampleHTML() string {
	state := map[string]any{
		"note": map[string]any{
			"currentNoteId": sampleNoteID,
			"noteDetailMap": map[string]any{
				sampleNoteID: map[string]any{
					"note": map[string]any{
						"noteId": sampleNoteID,
						"title":  "一杯好喝的奶茶",
						"desc":   "今天试了新出的桂花乌龙,三分糖刚刚好。\n\n#奶茶[话题]# #探店[话题]#",
						"type":   "normal",
						"time":   1762200000000,
						"user": map[string]any{
							"nickname": "奶茶小分队",
							"userId":   "user-1",
						},
						"imageList": []any{
							map[string]any{
								"urlDefault": "http://sns-webpic-qc.xhscdn.com/a.jpg",
								"width":      float64(1080),
								"height":     float64(1440),
							},
							map[string]any{
								"urlPre": "",
								"infoList": []any{
									map[string]any{"imageScene": "CRD_WM_WEBP", "url": "https://example.com/thumb.webp"},
									map[string]any{"imageScene": "WB_DFT", "url": "https://sns-img-bd.xhscdn.com/b.jpg"},
								},
							},
							map[string]any{"urlDefault": "http://sns-webpic-qc.xhscdn.com/a.jpg"},
						},
						"tagList": []any{
							map[string]any{"name": "奶茶"},
							map[string]any{"name": "#探店"},
						},
					},
				},
			},
		},
		"other": map[string]any{"empty": "undefined"},
	}
	data, _ := json.Marshal(state)
	// 模拟小红书页面里的 JS 字面量:undefined 不是合法 JSON。
	text := strings.ReplaceAll(string(data), `"empty":"undefined"`, `"empty":undefined`)
	return `<html><head></head><body><script>window.__INITIAL_STATE__=` + text + `;</script></body></html>`
}

func TestParseNoteHTML(t *testing.T) {
	note, err := ParseNoteHTML(sampleHTML(), sampleNoteID)
	if err != nil {
		t.Fatalf("ParseNoteHTML: %v", err)
	}
	if note.ID != sampleNoteID {
		t.Fatalf("id = %q", note.ID)
	}
	if note.Title != "一杯好喝的奶茶" {
		t.Fatalf("title = %q", note.Title)
	}
	if !strings.Contains(note.Desc, "桂花乌龙") {
		t.Fatalf("desc = %q", note.Desc)
	}
	if note.Author != "奶茶小分队" || note.AuthorID != "user-1" {
		t.Fatalf("author = %+v", note)
	}
	if note.PublishedAt != "2025-11-04" {
		t.Fatalf("published = %q", note.PublishedAt)
	}
	if len(note.Images) != 2 {
		t.Fatalf("images = %+v", note.Images)
	}
	if note.Images[0].URL != "https://sns-webpic-qc.xhscdn.com/a.jpg" {
		t.Fatalf("first image = %q", note.Images[0].URL)
	}
	if note.Images[0].Width != 1080 || note.Images[0].Height != 1440 {
		t.Fatalf("image size = %+v", note.Images[0])
	}
	if note.Images[1].URL != "https://sns-img-bd.xhscdn.com/b.jpg" {
		t.Fatalf("second image = %q", note.Images[1].URL)
	}
	if len(note.Tags) != 2 || note.Tags[0] != "奶茶" || note.Tags[1] != "探店" {
		t.Fatalf("tags = %+v", note.Tags)
	}
}

func TestParseNoteHTMLMobileState(t *testing.T) {
	state := `{"noteData":{"data":{"noteData":{"noteId":"` + sampleNoteID + `","title":"手机端笔记","desc":"手机端正文","imageList":[{"urlDefault":"//sns-webpic-qc.xhscdn.com/c.jpg"}]}}}}`
	html := `<script>window.__INITIAL_SSR_STATE__=` + state + `</script>`
	note, err := ParseNoteHTML(html, sampleNoteID)
	if err != nil {
		t.Fatalf("ParseNoteHTML: %v", err)
	}
	if note.Title != "手机端笔记" || len(note.Images) != 1 {
		t.Fatalf("unexpected note: %+v", note)
	}
	if note.Images[0].URL != "https://sns-webpic-qc.xhscdn.com/c.jpg" {
		t.Fatalf("image url = %q", note.Images[0].URL)
	}
}

func TestParseCapturedNote(t *testing.T) {
	raw := []byte(`{
	  "url": "https://www.xiaohongshu.com/explore/` + sampleNoteID + `",
	  "note": {
	    "noteId": "` + sampleNoteID + `",
	    "title": "注入按钮提交的笔记",
	    "desc": "从页面里读到的正文",
	    "time": 1762200000000,
	    "user": {"nickname": "页面作者"},
	    "imageList": [
	      {"urlDefault": "http://sns-webpic-qc.xhscdn.com/a.jpg", "width": 1080, "height": 1440},
	      {"infoList": [{"imageScene": "WB_DFT", "url": "https://sns-img-bd.xhscdn.com/b.jpg"}]}
	    ],
	    "tagList": [{"name": "奶茶"}, {"name": "探店"}]
	  }
	}`)
	note, err := ParseCapturedNote(raw, "")
	if err != nil {
		t.Fatalf("ParseCapturedNote: %v", err)
	}
	if note.ID != sampleNoteID || note.Title != "注入按钮提交的笔记" || note.Author != "页面作者" {
		t.Fatalf("unexpected note: %+v", note)
	}
	if note.URL != "" {
		t.Fatalf("url should be filled by caller, got %q", note.URL)
	}
	if len(note.Images) != 2 || note.Images[0].URL != "https://sns-webpic-qc.xhscdn.com/a.jpg" {
		t.Fatalf("images = %+v", note.Images)
	}
	if len(note.Tags) != 2 || note.Tags[1] != "探店" {
		t.Fatalf("tags = %+v", note.Tags)
	}
}

func TestParseNoteID(t *testing.T) {
	cases := map[string]string{
		"https://www.xiaohongshu.com/explore/" + sampleNoteID + "?xsec_token=abc":             sampleNoteID,
		"https://www.xiaohongshu.com/discovery/item/" + sampleNoteID + "?xsec_source=pc_user": sampleNoteID,
		"www.xiaohongshu.com/note/" + sampleNoteID:                                            sampleNoteID,
		"https://www.xiaohongshu.com/explore?noteId=" + sampleNoteID:                          sampleNoteID,
		sampleNoteID: sampleNoteID,
	}
	for raw, want := range cases {
		got, err := ParseNoteID(raw)
		if err != nil {
			t.Errorf("ParseNoteID(%q): %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("ParseNoteID(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestIsXHSURL(t *testing.T) {
	yes := []string{
		"https://www.xiaohongshu.com/explore/" + sampleNoteID,
		"http://xhslink.com/a/abcdef",
		"www.xiaohongshu.com/discovery/item/" + sampleNoteID,
	}
	for _, raw := range yes {
		if !IsXHSURL(raw) {
			t.Errorf("IsXHSURL(%q) = false", raw)
		}
	}
	no := []string{
		"https://mp.weixin.qq.com/s/abc",
		"https://example.com/explore/" + sampleNoteID,
		"",
	}
	for _, raw := range no {
		if IsXHSURL(raw) {
			t.Errorf("IsXHSURL(%q) = true", raw)
		}
	}
}

func TestStripTopicTags(t *testing.T) {
	input := "雀斑，是甜攒满的记号☀️\n" +
		"我们把带着雀斑的「维纳斯苹果」做进咖啡\n" +
		"2款#茶百道咖啡[话题]# 新品11月19日上新\n\n" +
		"#茶百道[话题]# #茶百道小雀斑美式[话题]# #茶百道小雀斑拿铁[话题]#\n\n" +
		"![图1](/uploads/posts/a.webp)"
	got := StripTopicTags(input)
	for _, banned := range []string{"[话题]#", "#茶百道", "#茶百道咖啡"} {
		if strings.Contains(got, banned) {
			t.Errorf("未删除 %q: %q", banned, got)
		}
	}
	if !strings.Contains(got, "2款新品11月19日上新") {
		t.Errorf("正文空格未清理: %q", got)
	}
	if !strings.Contains(got, "维纳斯苹果") || !strings.Contains(got, "![图1]") {
		t.Errorf("正文或图片被误删: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("空行未合并: %q", got)
	}
}

func TestExtractURL(t *testing.T) {
	const id = "6af55bb7000000002b01a361"
	cases := map[string]string{
		"https://www.xiaohongshu.com/discovery/item/" + id + "?xsec_token=abc": "https://www.xiaohongshu.com/discovery/item/" + id + "?xsec_token=abc",
		"看看这个 https://www.xiaohongshu.com/discovery/item/" + id + "?xsec_token=abc，复制后打开": "https://www.xiaohongshu.com/discovery/item/" + id + "?xsec_token=abc",
		"https://xhslink.com/a/abcdef":                              "https://xhslink.com/a/abcdef",
		"https://example.com/explore/" + id:                         "",
		"":                                                          "",
	}
	for input, want := range cases {
		if got := ExtractURL(input); got != want {
			t.Errorf("ExtractURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUserAgents(t *testing.T) {
	if !strings.Contains(DefaultUserAgent(), "iPhone") {
		t.Fatalf("默认 UA 应为移动端: %q", DefaultUserAgent())
	}

	c := &Client{}
	agents := c.userAgents()
	if len(agents) != 2 || agents[0] != defaultUserAgent || agents[1] != desktopUserAgent {
		t.Fatalf("默认 UA 回退顺序 = %v", agents)
	}

	c.UserAgent = "custom-ua"
	agents = c.userAgents()
	if len(agents) != 1 || agents[0] != "custom-ua" {
		t.Fatalf("显式 UA 不应被覆盖: %v", agents)
	}
}

func TestSanitizeJSJSON(t *testing.T) {
	input := `{"a":undefined,"b":"undefined","c":NaN,"d":-Infinity,"e":1}`
	output := sanitizeJSJSON(input)
	want := `{"a":null,"b":"undefined","c":null,"d":null,"e":1}`
	if output != want {
		t.Fatalf("sanitizeJSJSON = %q, want %q", output, want)
	}
}

func TestFindRedirectInHTML(t *testing.T) {
	html := `<script>window.location.href = "https://www.xiaohongshu.com/explore/` + sampleNoteID + `"</script>`
	got := findRedirectInHTML(html, "https://xhslink.com/a/abc")
	if !strings.Contains(got, sampleNoteID) {
		t.Fatalf("redirect = %q", got)
	}

	meta := `<meta http-equiv="refresh" content="0; url=https://www.xiaohongshu.com/explore/` + sampleNoteID + `">`
	got = findRedirectInHTML(meta, "https://xhslink.com/a/abc")
	if !strings.Contains(got, sampleNoteID) {
		t.Fatalf("meta redirect = %q", got)
	}
}
