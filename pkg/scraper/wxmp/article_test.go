package wxmp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newHTMLServer 返回一个固定 HTML 的测试服务器。
func newHTMLServer(html string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, html)
	}))
}

// TestFetchArticleRejectsVerificationPage 验证微信安全验证页不会被当成正常文章。
func TestFetchArticleRejectsVerificationPage(t *testing.T) {
	srv := newHTMLServer(`<!DOCTYPE html><html><head><title></title></head><body>
<div class="weui-msg"></div>
<script>var PAGE_MID='mmbizwap:secitptpage/verify.html';</script>
</body></html>`)
	defer srv.Close()

	dl := &OfficialAccountDownload{}
	_, err := dl.FetchArticle(srv.URL + "/s/test")
	if err == nil {
		t.Fatal("验证页应当返回错误")
	}
	if !strings.Contains(err.Error(), "verification") {
		t.Errorf("错误信息未说明是验证页: %v", err)
	}
}

// TestFetchArticleRejectsEmptyPage 验证没有任何文章内容的页面会报错而不是归档空文章。
func TestFetchArticleRejectsEmptyPage(t *testing.T) {
	srv := newHTMLServer(`<!DOCTYPE html><html><head><title></title></head><body><p>参数错误</p></body></html>`)
	defer srv.Close()

	dl := &OfficialAccountDownload{}
	_, err := dl.FetchArticle(srv.URL + "/s/test")
	if err == nil {
		t.Fatal("空页面应当返回错误")
	}
	if !strings.Contains(err.Error(), "未能解析文章内容") {
		t.Errorf("错误信息不符合预期: %v", err)
	}
}

// TestFetchArticleParsesCgiData 验证正常文章页仍能解析出标题与正文。
func TestFetchArticleParsesCgiData(t *testing.T) {
	srv := newHTMLServer(`<!DOCTYPE html><html><body>
<div id="js_content"><p>正文</p></div>
<script>
window.cgiDataNew = {
  "title": "测试标题",
  "nick_name": "测试号",
  "content_noencode": "<p>正文内容</p>",
  "create_time": "1760000000",
  "cdn_url": "https://mmbiz.qpic.cn/cover.jpg"
};
</script>
</body></html>`)
	defer srv.Close()

	dl := &OfficialAccountDownload{}
	article, err := dl.FetchArticle(srv.URL + "/s/test")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if article.Title != "测试标题" || !strings.Contains(article.Content, "正文内容") {
		t.Errorf("文章 = %+v", article)
	}
	if article.AuthorNickname != "测试号" {
		t.Errorf("公众号名 = %q", article.AuthorNickname)
	}
	if article.PublishTimeStr == "" {
		t.Errorf("发布时间为空")
	}
}

// TestIsEmptyArticle 验证空文章判定。
func TestIsEmptyArticle(t *testing.T) {
	cases := []struct {
		name    string
		article *WechatOfficialArticle
		want    bool
	}{
		{"nil", nil, true},
		{"全空", &WechatOfficialArticle{}, true},
		{"有标题", &WechatOfficialArticle{Title: "t"}, false},
		{"有正文", &WechatOfficialArticle{Content: "<p>x</p>"}, false},
		{"有图片", &WechatOfficialArticle{Images: []string{"a"}}, false},
		{"有图集", &WechatOfficialArticle{PicturePageInfoList: []PicturePageInfo{{CdnURL: "a"}}}, false},
	}
	for _, c := range cases {
		if got := isEmptyArticle(c.article); got != c.want {
			t.Errorf("%s: isEmptyArticle = %v, 期望 %v", c.name, got, c.want)
		}
	}
}
