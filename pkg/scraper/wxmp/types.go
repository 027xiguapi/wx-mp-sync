// Package wxmp 实现微信公众号文章的抓取、解析与本地归档。
// 结构移植自 wx_channels_download-main/pkg/scraper/wxmp。
package wxmp

import (
	"encoding/json"
	"strconv"
	"strings"
)

// FlexibleInt 兼容微信返回的整型字段可能是数字也可能是字符串的情况。
type FlexibleInt int64

func (v *FlexibleInt) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*v = FlexibleInt(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		*v = 0
		return nil
	}
	parsed, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*v = FlexibleInt(parsed)
	return nil
}

// CgiDataNew 对应公众号文章页中的 window.cgiDataNew 对象。
type CgiDataNew struct {
	BaseResp struct {
		Ret         int    `json:"ret"`
		ErrMsg      string `json:"errmsg"`
		WxToken     int    `json:"wxtoken"`
		CookieCount int    `json:"cookie_count"`
		SessionID   string `json:"sessionid"`
	} `json:"base_resp"`
	UserName        string      `json:"user_name"`
	NickName        string      `json:"nick_name"`
	RoundHeadImg    string      `json:"round_head_img"`
	Title           string      `json:"title"`
	Desc            string      `json:"desc"`
	ContentNoEncode string      `json:"content_noencode"`
	CreateTime      string      `json:"create_time"`
	CdnURL          string      `json:"cdn_url"`
	Link            string      `json:"link"`
	SourceURL       string      `json:"source_url"`
	CanShare        int         `json:"can_share"`
	Alias           string      `json:"alias"`
	Type            int         `json:"type"`
	Author          string      `json:"author"`
	IsLimitUser     int         `json:"is_limit_user"`
	ShowCoverPic    int         `json:"show_cover_pic"`
	AdvertisementNum int        `json:"advertisement_num"`
	AdvertisementInfo []any     `json:"advertisement_info"`
	OriCreateTime   FlexibleInt `json:"ori_create_time"`
	UserUin         FlexibleInt `json:"user_uin"`
	TotalItemNum    int         `json:"total_item_num"`
	IsAsync         int         `json:"is_async"`
	CommentID       string      `json:"comment_id"`
	ImgFormat       string      `json:"img_format"`
	SvrTime         FlexibleInt `json:"svr_time"`
	CopyrightInfo   struct {
		CopyrightStat      int `json:"copyright_stat"`
		IsCartoonCopyright int `json:"is_cartoon_copyright"`
	} `json:"copyright_info"`
	CanReward        int    `json:"can_reward"`
	Signature        string `json:"signature"`
	RewardWording    string `json:"reward_wording"`
	InMm             int    `json:"in_mm"`
	AppID            string `json:"app_id"`
	ShowComment      int    `json:"show_comment"`
	CanUsePage       int    `json:"can_use_page"`
	HdHeadImg        string `json:"hd_head_img"`
	DelReasonID      int    `json:"del_reason_id"`
	SrcID            string `json:"srcid"`
	IsWxgStuffUin    int    `json:"is_wxg_stuff_uin"`
	NeedReportCost   int    `json:"need_report_cost"`
	BizUin           string `json:"bizuin"`
	Mid              FlexibleInt `json:"mid"`
	Idx              FlexibleInt `json:"idx"`
	Sn               string `json:"sn"`
	UseTxVideoPlayer int    `json:"use_tx_video_player"`
	IsOnlyRead       int    `json:"is_only_read"`
	ReqID            string `json:"req_id"`
	UseOuterLink     int    `json:"use_outer_link"`
	BanScene         int    `json:"ban_scene"`
	CspNonceStr      int    `json:"csp_nonce_str"`
	MsgDailyIdx      FlexibleInt `json:"msg_daily_idx"`
	OriHeadImgURL    string `json:"ori_head_img_url"`
	FilterTime       FlexibleInt `json:"filter_time"`
	AppmsgFeFilter   string `json:"appmsg_fe_filter"`
	IsLogin          int    `json:"is_login"`
	RewardMoney      int    `json:"reward_money"`
	PageType         int    `json:"page_type"`
	ItemShowType     int    `json:"item_show_type"`
	VoiceInAppmsg    []any  `json:"voice_in_appmsg"`
	VideoPageInfo    struct {
		MpVideoTransInfo []any `json:"mp_video_trans_info"`
		DramaVideoInfo   struct{} `json:"drama_video_info"`
		DramaInfo        struct{} `json:"drama_info"`
	} `json:"video_page_info"`
	VideoPageInfos      []VideoPageInfoItem `json:"video_page_infos"`
	PicturePageInfoList []PicturePageInfo   `json:"picture_page_info_list"`
}

// VideoPageInfoItem 描述文章内嵌视频的信息。
type VideoPageInfoItem struct {
	VideoID              string             `json:"video_id"`
	OriStatus            int                `json:"ori_status"`
	SourceBizuin         string             `json:"source_bizuin"`
	SourceNickname       string             `json:"source_nickname"`
	SourceLink           string             `json:"source_link"`
	IsMpVideo            int                `json:"is_mp_video"`
	IsMpVideoDelete      int                `json:"is_mp_video_delete"`
	MpVideoTransInfo     []MpVideoTransInfo `json:"mp_video_trans_info"`
	ContentNoencode      string             `json:"content_noencode"`
	IsAppmsgUnauthorized int                `json:"is_appmsg_unauthorized"`
	CoverURL             string             `json:"cover_url"`
	VideoidUploadtime    int                `json:"videoid_uploadtime"`
	VideoidBizuin        int                `json:"videoid_bizuin"`
}

// MpVideoTransInfo 描述视频的清晰度源。
type MpVideoTransInfo struct {
	DurationMs          int    `json:"duration_ms"`
	Filesize            any    `json:"filesize"`
	FormatID            int    `json:"format_id"`
	Height              int    `json:"height"`
	URL                 string `json:"url"`
	VideoQualityLevel   int    `json:"video_quality_level"`
	VideoQualityWording string `json:"video_quality_wording"`
	Width               int    `json:"width"`
}

// PictureLivePhotoFormat 描述图集 live photo 的视频源。
type PictureLivePhotoFormat struct {
	FormatID int         `json:"format_id"`
	URL      string      `json:"url"`
	FileSize FlexibleInt `json:"file_size"`
	Duration int64       `json:"duration"`
	Width    int         `json:"width"`
	Height   int         `json:"height"`
}

// PictureLivePhoto 描述图集中的 live photo 信息。
type PictureLivePhoto struct {
	Vid        string                   `json:"vid"`
	Type       int                      `json:"type"`
	FormatInfo []PictureLivePhotoFormat `json:"format_info"`
}

// PicturePageInfo 描述图集文章中的一张图片。
type PicturePageInfo struct {
	CdnURL      string           `json:"cdn_url"`
	Width       int              `json:"width"`
	Height      int              `json:"height"`
	ThemeColor  string           `json:"theme_color"`
	LivePhoto   PictureLivePhoto `json:"live_photo"`
	IsQrCode    int              `json:"is_qr_code"`
}

// WechatOfficialArticle 是抓取解析后的文章统一结构。
type WechatOfficialArticle struct {
	Type                int                 `json:"type"`
	Title               string              `json:"title"`
	Content             string              `json:"content"`
	ContentLength       int                 `json:"content_length"`
	Images              []string            `json:"images"`
	Creator             string              `json:"creator"`
	AuthorNickname      string              `json:"author_nickname"`
	AuthorAvatar        string              `json:"author_avatar"`
	AuthorID            string              `json:"author_id"`
	PublishTimeStr      string              `json:"publish_time_str"`
	Videos              []VideoPageInfoItem `json:"videos"`
	PicturePageInfoList []PicturePageInfo   `json:"picture_page_info_list,omitempty"`
	PageJSON            *CgiDataNew         `json:"-"`
	PageHTML            string              `json:"-"`
}
