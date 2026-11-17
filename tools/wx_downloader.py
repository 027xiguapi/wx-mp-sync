# -*- coding: utf-8 -*-
"""
微信公众号文章搜索 + 批量下载工具。

流程:
  1. 用搜狗微信搜索(keyword)拿到文章列表(含账号名、发布时间、跳转链接)
  2. 解析 sogou /link 跳转,还原出 mp.weixin.qq.com 真实链接
  3. 逐篇下载正文 HTML、Markdown、图片、封面、metadata.json 到本地目录

用法:
  python tools/wx_downloader.py "全球机场地图" --max 20 --out ./downloads
"""
import argparse
import hashlib
import html
import json
import os
import random
import re
import sys
import time
from urllib.parse import urljoin, urlparse

import requests
import urllib3

urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36")


def log(msg):
    print(msg, flush=True)


def new_session():
    s = requests.Session()
    s.verify = False  # 本机有 MITM 代理(见 wx-mp-sync),公共文章抓取不做证书校验
    s.headers.update({
        "User-Agent": UA,
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Referer": "https://weixin.sogou.com/",
    })
    return s


# --------------------------------------------------------------------------
# 1. 搜索
# --------------------------------------------------------------------------
ITEM_RE = re.compile(r'<li id="sogou_vr_11002601_box_\d+".*?(?=<li id="sogou_vr_11002601_box_|</ul>)', re.S)


def _text(fragment):
    return html.unescape(re.sub(r"<[^>]+>", "", fragment)).replace("\u200b", "").strip()


def parse_items(page_html):
    items = []
    for block in ITEM_RE.findall(page_html):
        m = re.search(r'<h3>\s*<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>', block, re.S)
        if not m:
            continue
        href, title = m.group(1), _text(m.group(2))
        if not href or not title:
            continue
        m_acct = (re.search(r'<span class="all-time-y2">(.*?)</span>', block, re.S)
                  or re.search(r'<a[^>]*class="account"[^>]*>(.*?)</a>', block, re.S))
        account = _text(m_acct.group(1)) if m_acct else ""
        m_ts = re.search(r"timeConvert\('(\d+)'\)", block)
        ts = int(m_ts.group(1)) if m_ts else 0
        m_sum = re.search(r'<p class="txt-info"[^>]*>(.*?)</p>', block, re.S)
        summary = _text(m_sum.group(1)) if m_sum else ""
        items.append({
            "title": title,
            "account": account,
            "ts": ts,
            "date": time.strftime("%Y-%m-%d", time.localtime(ts)) if ts else "",
            "summary": summary[:150],
            "sogou_url": urljoin("https://weixin.sogou.com/", href.replace("&amp;", "&")),
        })
    return items


def search(session, keyword, pages=1, tsn=None):
    all_items, seen = [], set()
    for page in range(1, pages + 1):
        params = {"type": 2, "query": keyword, "ie": "utf8", "page": page}
        if tsn:
            params["tsn"] = tsn
        r = session.get("https://weixin.sogou.com/weixin", params=params, timeout=30)
        if r.status_code != 200:
            log(f"  [搜索] 第{page}页 HTTP {r.status_code}")
            break
        if "请输入验证码" in r.text or "antispider" in r.url:
            log("  [搜索] 触发搜狗验证码,停止翻页")
            break
        items = parse_items(r.text)
        log(f"  [搜索] 第{page}页 解析到 {len(items)} 条")
        for it in items:
            key = (it["title"], it["account"])
            if key in seen:
                continue
            seen.add(key)
            all_items.append(it)
        if len(items) < 5:
            break
        time.sleep(random.uniform(1.5, 2.5))
    return all_items


# --------------------------------------------------------------------------
# 2. 还原 sogou 跳转链接
# --------------------------------------------------------------------------
def resolve_url(session, sogou_url):
    r = session.get(sogou_url, timeout=30, headers={"Referer": "https://weixin.sogou.com/"})
    if r.status_code != 200:
        raise RuntimeError(f"跳转页 HTTP {r.status_code}")
    if "antispider" in r.url or "请输入验证码" in r.text:
        raise RuntimeError("搜狗反爬:需要验证码")
    parts = re.findall(r"url \+= '([^']*)'", r.text)
    if not parts:
        m = re.search(r'window\.location\.replace\("([^"]+)"\)', r.text)
        if m:
            return m.group(1).replace("\\x26", "&")
        raise RuntimeError("未能解析跳转链接")
    real = "".join(parts).replace("&amp;", "&").replace("@", "")
    if real.startswith("/link?"):
        raise RuntimeError("跳转失败")
    return real


# --------------------------------------------------------------------------
# 3. 下载与解析文章
# --------------------------------------------------------------------------
def sanitize(name, limit=80):
    name = re.sub(r'[\\/:*?"<>|\r\n\t]+', "_", name).strip(" .")
    return (name[:limit] or "untitled").strip()


def pick(pattern, text, default=""):
    m = re.search(pattern, text, re.S)
    return html.unescape(m.group(1)).strip() if m else default


def extract_article(page):
    title = pick(r'<h1[^>]*id="activity-name"[^>]*>(.*?)</h1>', page)
    if not title:
        title = pick(r'var msg_title = [\'"](.*?)[\'"];', page)
    author = pick(r'<a[^>]*id="js_name"[^>]*>(.*?)</a>', page)
    if not author:
        author = pick(r'var nickname = [\'"](.*?)[\'"];', page)
    account_id = pick(r'var user_name = [\'"](.*?)[\'"];', page)
    publish = pick(r'var createTime = \'(.*?)\';', page) or pick(r'<em id="publish_time"[^>]*>(.*?)</em>', page)
    cover = pick(r'var msg_cdn_url = "(.*?)";', page)
    desc = pick(r'var msg_desc = "(.*?)";', page)
    m = re.search(r'<div class="rich_media_content[^"]*"[^>]*id="js_content"[^>]*>(.*?)</div>\s*<script', page, re.S)
    if not m:
        m = re.search(r'<div[^>]*id="js_content"[^>]*>(.*?)</div>', page, re.S)
    content = m.group(1) if m else ""
    title = html.unescape(re.sub(r"<[^>]+>", "", title)).strip()
    author = html.unescape(re.sub(r"<[^>]+>", "", author)).strip()
    return {
        "title": title, "author": author, "account_id": account_id,
        "publish": publish, "cover": cover, "desc": desc, "content": content,
    }


IMG_RE = re.compile(r'<img\b[^>]*>', re.I)


def normalize_images(content):
    """把 data-src 提升为 src,返回 (html, [url...])。"""
    urls = []

    def repl(m):
        tag = m.group(0)
        src = re.search(r'\bdata-src="([^"]+)"', tag) or re.search(r'\bsrc="([^"]+)"', tag)
        if not src:
            return tag
        u = html.unescape(src.group(1))
        if u.startswith("//"):
            u = "https:" + u
        if u.startswith("http"):
            urls.append(u)
        return re.sub(r'\bsrc="[^"]*"', f'src="{u}"', tag) if 'src=' in tag else tag[:-1] + f' src="{u}">'

    new = IMG_RE.sub(repl, content)
    return new, list(dict.fromkeys(urls))


def content_to_markdown(content_html, image_map):
    from html.parser import HTMLParser

    class Conv(HTMLParser):
        def __init__(self):
            super().__init__(convert_charrefs=True)
            self.out, self.skip, self.list_stack, self.in_pre = [], 0, [], False

        def handle_starttag(self, tag, attrs):
            a = dict(attrs)
            if tag in ("script", "style"):
                self.skip += 1
            elif tag == "br":
                self.out.append("\n")
            elif tag in ("p", "section", "div", "tr"):
                if self.out and not self.out[-1].endswith("\n\n"):
                    self.out.append("\n\n")
            elif tag in ("h1", "h2", "h3", "h4", "h5", "h6"):
                self.out.append("\n\n" + "#" * int(tag[1]) + " ")
            elif tag == "li":
                self.out.append("\n" + "  " * len(self.list_stack) + "- ")
            elif tag in ("ul", "ol"):
                self.list_stack.append(tag)
            elif tag == "blockquote":
                self.out.append("\n\n> ")
            elif tag == "pre":
                self.in_pre = True
                self.out.append("\n\n```\n")
            elif tag in ("strong", "b"):
                self.out.append("**")
            elif tag in ("em", "i"):
                self.out.append("*")
            elif tag == "img":
                src = a.get("data-src") or a.get("src") or ""
                if src:
                    local = image_map.get(src, src)
                    self.out.append(f"\n\n![image]({local})\n\n")
            elif tag == "a":
                self.out.append("[")

        def handle_endtag(self, tag):
            if tag in ("script", "style"):
                self.skip = max(0, self.skip - 1)
            elif tag in ("h1", "h2", "h3", "h4", "h5", "h6", "p", "section", "div"):
                if self.out and not self.out[-1].endswith("\n\n"):
                    self.out.append("\n\n")
            elif tag in ("ul", "ol") and self.list_stack:
                self.list_stack.pop()
            elif tag == "pre":
                self.in_pre = False
                self.out.append("\n```\n\n")
            elif tag in ("strong", "b"):
                self.out.append("**")
            elif tag in ("em", "i"):
                self.out.append("*")
            elif tag == "a":
                self.out.append(")")

        def handle_data(self, data):
            if self.skip:
                return
            text = re.sub(r"[ \t\r\f\v]+", " ", data)
            if not self.in_pre:
                text = re.sub(r"\n+", " ", text)
            self.out.append(text)

    c = Conv()
    c.feed(content_html)
    md = "".join(c.out)
    md = re.sub(r"[ \t]+\n", "\n", md)
    md = re.sub(r"\n{3,}", "\n\n", md)
    return md.strip()


def save_image(session, url, folder, referer):
    ext = os.path.splitext(urlparse(url).path)[1].lower()
    if ext not in (".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp"):
        ext = ".jpg"
    name = hashlib.md5(url.encode()).hexdigest()[:16] + ext
    path = os.path.join(folder, name)
    if os.path.exists(path) and os.path.getsize(path) > 0:
        return path
    r = session.get(url, timeout=40, headers={"Referer": referer, "User-Agent": UA})
    r.raise_for_status()
    with open(path, "wb") as f:
        f.write(r.content)
    return path


def download_one(session, url, base_dir, index):
    r = session.get(url, timeout=40, headers={"User-Agent": UA})
    r.raise_for_status()
    if "该内容已被发布者删除" in r.text or "此内容因违规无法查看" in r.text:
        raise RuntimeError("文章不可见(已删除/违规)")
    art = extract_article(r.text)
    if not art["title"]:
        raise RuntimeError("未能提取标题")
    date = (art["publish"] or "")[:10] or time.strftime("%Y-%m-%d")
    folder = os.path.join(base_dir, f"{date}_{sanitize(art['author'] or 'unknown', 20)}_{sanitize(art['title'])}")
    os.makedirs(folder, exist_ok=True)
    img_dir = os.path.join(folder, "images")
    os.makedirs(img_dir, exist_ok=True)

    content, img_urls = normalize_images(art["content"])
    image_map, ok, fail = {}, 0, 0
    for u in img_urls:
        try:
            p = save_image(session, u, img_dir, url)
            image_map[u] = os.path.relpath(p, folder).replace("\\", "/")
            ok += 1
        except Exception:
            fail += 1
    if art["cover"]:
        try:
            p = save_image(session, art["cover"], folder, url)
            image_map[art["cover"]] = os.path.relpath(p, folder).replace("\\", "/")
        except Exception:
            pass

    md = content_to_markdown(content, image_map)
    for u, rel in image_map.items():
        content = content.replace(f'src="{u}"', f'src="{rel}"')

    page_html = f"""<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>{html.escape(art['title'])}</title>
<meta name="author" content="{html.escape(art['author'])}">
<meta name="publish" content="{html.escape(art['publish'])}">
<style>body{{max-width:780px;margin:40px auto;padding:0 18px;font:16px/1.75 -apple-system,"PingFang SC","Microsoft YaHei",sans-serif;color:#222}}
img{{max-width:100%;height:auto}}</style></head>
<body><h1>{html.escape(art['title'])}</h1>
<p>{html.escape(art['author'])} · {html.escape(art['publish'])}</p>
<article>{content}</article>
<p><a href="{html.escape(url)}">原文链接</a></p></body></html>
"""
    with open(os.path.join(folder, "index.html"), "w", encoding="utf-8") as f:
        f.write(page_html)
    with open(os.path.join(folder, sanitize(art["title"]) + ".md"), "w", encoding="utf-8") as f:
        head = f"# {art['title']}\n\n> {art['author']} · {art['publish']}\n> 原文:{url}\n\n"
        f.write(head + md + "\n")
    meta = {
        "index": index,
        "title": art["title"],
        "author": art["author"],
        "account_id": art["account_id"],
        "publish": art["publish"],
        "url": url,
        "cover": os.path.basename(image_map.get(art["cover"], "")),
        "images": len(image_map),
        "images_failed": fail,
        "downloaded_at": time.strftime("%Y-%m-%d %H:%M:%S"),
    }
    with open(os.path.join(folder, "metadata.json"), "w", encoding="utf-8") as f:
        json.dump(meta, f, ensure_ascii=False, indent=2)
    return folder, ok + (1 if art["cover"] else 0), fail


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("keyword")
    ap.add_argument("--pages", type=int, default=1)
    ap.add_argument("--max", type=int, default=20, help="最多下载篇数")
    ap.add_argument("--out", default="./downloads")
    ap.add_argument("--tsn", default=None, help="1天 2周 3月 4年; 默认不限")
    ap.add_argument("--account", default=None, help="只下载该公众号(账号名)的文章")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    s = new_session()
    log(f"[1/3] 搜狗微信搜索:{args.keyword}")
    items = search(s, args.keyword, args.pages, args.tsn)
    if args.account:
        items = [it for it in items if args.account in it["account"]]
        log(f"      账号过滤「{args.account}」后剩 {len(items)} 条")
    items.sort(key=lambda x: x["ts"], reverse=True)
    items = items[: args.max]
    log(f"      共 {len(items)} 条待处理")

    base_dir = os.path.join(args.out, sanitize(args.keyword))
    os.makedirs(base_dir, exist_ok=True)
    manifest = []
    ok = fail = skip = 0
    existing = set(os.listdir(base_dir)) if os.path.isdir(base_dir) else set()
    for i, it in enumerate(items, 1):
        log(f"[{i}/{len(items)}] {it['date']} {it['account']} | {it['title']}")
        prefix = f"{it['date']}_"
        suffix = "_" + sanitize(it["title"])
        if any(name.startswith(prefix) and name.endswith(suffix) for name in existing):
            log("    · 已存在,跳过")
            skip += 1
            continue
        try:
            real = resolve_url(s, it["sogou_url"])
        except Exception as e:
            log(f"    ✗ 跳转失败:{e}")
            fail += 1
            manifest.append({**it, "error": str(e)})
            time.sleep(random.uniform(2, 4))
            continue
        if args.dry_run:
            log(f"    → {real[:110]}")
            manifest.append({**it, "url": real})
            time.sleep(random.uniform(1.5, 2.5))
            continue
        try:
            folder, nimg, nfail = download_one(s, real, base_dir, i)
            log(f"    ✓ {folder}  图片 {nimg} 张(失败 {nfail})")
            ok += 1
            manifest.append({**it, "url": real, "folder": folder})
        except Exception as e:
            log(f"    ✗ {e}")
            fail += 1
            manifest.append({**it, "url": real, "error": str(e)})
        time.sleep(random.uniform(2, 4))

    with open(os.path.join(base_dir, "_manifest.json"), "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)
    log(f"完成:成功 {ok} 篇,失败 {fail} 篇 → {os.path.abspath(base_dir)}")


if __name__ == "__main__":
    sys.exit(main())
