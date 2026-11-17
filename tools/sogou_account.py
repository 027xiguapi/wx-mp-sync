# -*- coding: utf-8 -*-
"""列出搜狗微信“公众号”搜索结果(type=1)。"""
import html
import re
import sys

from wx_downloader import new_session, log

BLOCK_RE = re.compile(r'<li id="sogou_vr_10001_box_\d+".*?(?=<li id="sogou_vr_10001_box_|</ul>)', re.S)


def main(kw):
    s = new_session()
    r = s.get("https://weixin.sogou.com/weixin",
              params={"type": 1, "query": kw, "ie": "utf8"}, timeout=30)
    log(f"status={r.status_code} len={len(r.text)}")
    blocks = BLOCK_RE.findall(r.text)
    log(f"blocks={len(blocks)}")
    for b in blocks:
        name = re.search(r'<p class="tit">\s*<a[^>]*>(.*?)</a>', b, re.S) or re.search(r'<a[^>]*class="account"[^>]*>(.*?)</a>', b, re.S)
        weid = re.search(r'<label name="em_weixinhao">(.*?)</label>', b, re.S)
        desc = re.search(r'<dl[^>]*>(.*?)</dl>', b, re.S)
        txt = html.unescape(re.sub(r"<[^>]+>", "", name.group(1))).strip() if name else "?"
        print(f"- {txt}  [{html.unescape(re.sub(r'<[^>]+>', '', weid.group(1))).strip() if weid else ''}]")
        if desc:
            d = html.unescape(re.sub(r"<[^>]+>", " ", desc.group(1)))
            print("   ", re.sub(r"\s+", " ", d).strip()[:100])


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "全球机场地图")
