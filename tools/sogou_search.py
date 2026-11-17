# -*- coding: utf-8 -*-
"""Sogou 微信文章搜索的解析工具(仅解析,不写文件内容)。"""
import html
import re
import sys
import time

import requests

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36")


def new_session():
    s = requests.Session()
    s.headers.update({
        "User-Agent": UA,
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Referer": "https://weixin.sogou.com/",
    })
    return s


ITEM_RE = re.compile(r'<li id="sogou_vr_11002601_box_\d+".*?</li>', re.S)


def parse_results(page_html):
    items = []
    for block in ITEM_RE.findall(page_html):
        m_title = re.search(r'<h3>\s*<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>', block, re.S)
        if not m_title:
            continue
        href, title_html = m_title.group(1), m_title.group(2)
        title = html.unescape(re.sub(r"<[^>]+>", "", title_html)).strip()
        m_acct = re.search(r'class="account"[^>]*>(.*?)</a>', block, re.S)
        account = html.unescape(re.sub(r"<[^>]+>", "", m_acct.group(1))).strip() if m_acct else ""
        m_time = re.search(r'<span class="s2">(.*?)</span>', block, re.S)
        ts = html.unescape(m_time.group(1)).strip() if m_time else ""
        m_sum = re.search(r'<p class="txt-info"[^>]*>(.*?)</p>', block, re.S)
        summary = html.unescape(re.sub(r"<[^>]+>", "", m_sum.group(1))).strip() if m_sum else ""
        items.append({
            "title": title,
            "account": account,
            "time": ts,
            "summary": summary[:120],
            "href": href if href.startswith("http") else "https://weixin.sogou.com" + href,
        })
    return items


def search(session, query, page=1, search_type=2, time_range=None):
    params = {"type": search_type, "query": query, "ie": "utf8", "page": page}
    if time_range:
        params["tsn"] = time_range  # 1=一天 2=一周 3=一月 4=一年 5=自定义
    r = session.get("https://weixin.sogou.com/weixin", params=params, timeout=30)
    return r


if __name__ == "__main__":
    q = sys.argv[1] if len(sys.argv) > 1 else "全球机场地图"
    s = new_session()
    r = search(s, q)
    print("status", r.status_code, "len", len(r.text))
    m = re.search(r"<title>(.*?)</title>", r.text, re.S)
    print("title", html.unescape(m.group(1)).strip() if m else "?")
    res = parse_results(r.text)
    print("items", len(res))
    for i, it in enumerate(res[:20], 1):
        print(f"{i:2d}. [{it['time']}] {it['account']} | {it['title']}")
        print(f"    {it['href'][:140]}")
