# -*- coding: utf-8 -*-
"""根据已下载文章目录生成索引 _index.md。"""
import json
import os

BASE = os.path.join("downloads", "全球机场")


def main():
    rows = []
    for name in sorted(os.listdir(BASE)):
        d = os.path.join(BASE, name)
        meta_path = os.path.join(d, "metadata.json")
        if not os.path.isfile(meta_path):
            continue
        with open(meta_path, encoding="utf-8") as f:
            m = json.load(f)
        rows.append((m.get("publish", ""), m.get("title", ""), m.get("author", ""), name, m.get("images", 0)))
    rows.sort(reverse=True)
    lines = ["# 已下载文章索引(全球机场)\n",
             f"共 {len(rows)} 篇,每篇一个文件夹(含 index.html / Markdown / images / metadata.json)。\n",
             "| 发布日期 | 标题 | 公众号 | 图片 | 本地目录 |",
             "|---|---|---|---|---|"]
    for pub, title, author, folder, nimg in rows:
        lines.append(f"| {pub[:10]} | [{title}]({folder}/index.html) | {author} | {nimg} | `{folder}` |")
    out = os.path.join(BASE, "_index.md")
    with open(out, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    print("wrote", out, len(rows), "rows")


if __name__ == "__main__":
    main()
