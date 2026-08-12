# wx-mp-sync 公众号文章同步插件

用 Go 编写的微信公众号文章同步插件。核心功能:**在微信 PC 客户端浏览公众号文章时,页面右下角出现「⬇ 同步」按钮,点击即可把文章(HTML + Markdown + 图片)一键归档到本地**,并配套一个本地 Web 管理页。

实现参考 [`wx_channels_download-main`](wx_channels_download-main):本地起一个 MITM 代理(`ltaoo/echo`,纯 Go,无需 CGO),拦截 `mp.weixin.qq.com` 的页面响应,在 `</body>` 前注入按钮;点按钮后页面把文章数据 POST 到本地管理服务归档。

## 功能

- **微信内一键同步**:打开公众号文章 → 右下角「⬇ 同步」→ 自动归档
- **HTTPS 注入**:自动生成并安装本地根证书(MITM),自动设置系统代理,退出时自动恢复
- **双格式归档**:完整 HTML(图片本地化)+ Markdown,另存封面与 `metadata.json`
- **本地 Web 管理页**:查看已同步文章、手动粘贴链接同步、看运行日志
- **去重**:重复同步自动跳过
- 另保留**命令行同步**模式(`sync` / `watch`),无需微信也能批量抓取

## 构建

```bash
go build -o wx-mp-sync.exe .
```

## 使用(注入模式,主用法)

```bash
# 直接运行(首次会弹出一次 UAC 授权安装证书,并自动设置系统代理)
wx-mp-sync.exe

# 可选参数
wx-mp-sync.exe --proxy-port 2023 --admin-port 8080 --output ./articles
```

运行后:

1. 首次启动弹出 UAC,点击「是」安装根证书(HTTPS 注入必需)。
2. 程序自动把系统代理设为 `127.0.0.1:2023`(退出时自动恢复原有代理)。
3. 打开**微信 PC 客户端**,进入任意一篇公众号文章。
4. 页面右下角出现绿色「⬇ 同步」按钮 → 点击。
5. 按钮变为「✓ 已保存」,文章归档到输出目录。
6. 浏览器打开管理页 `http://127.0.0.1:8080` 查看已同步文章、手动同步、看日志。

### 相关参数

| 参数 | 说明 |
|---|---|
| `--proxy-port` | MITM 代理端口(默认 2023) |
| `--admin-port` | 管理页端口(默认 8080) |
| `--output` | 文章归档目录 |
| `--no-proxy` | 不自动设置系统代理(需手动把代理指向代理端口) |
| `--no-install-cert` | 不自动安装根证书 |
| `--uninstall-cert` | 卸载根证书后退出 |
| `--inject-match` | 注入匹配的域名(默认 `mp.weixin.qq.com`,一般无需改) |

## 推送到 bot 服务(可选)

在 `config.yaml` 里配置 `bot_sync`,每次同步(点按钮或手动)后会把文章推送到你的 bot 接口:

```yaml
bot_sync:
  enabled: true
  api_base: http://localhost:7000   # bot 服务地址
  bot_id: "your-bot-id"             # botId
  type: article
  # 可选:图床上传接口;留空则正文直接用微信原图 URL
  upload_api: /api/bot/sync/images  # 插件专用图床接口(Multipart: file,无需鉴权)
  upload_field: file
  cookie: ""                        # 一般不需要;若接口要登录鉴权才填浏览器 Cookie
```

**图床上传契约**(已适配你的 `POST /api/bot/sync/images`):
- 请求:Multipart `file`(MD5 由服务端计算,无需插件传)
- 鉴权:插件专用接口无会话鉴权,`cookie` 留空即可
- 返回:`{success:true, data:{url, thumbUrl,...}}`,程序取 `data.url` 做封面
- **正文图片替换为自定义标签**:
  ```
  <CustomImage src="{thumbUrl}" fullsrc="{url}" alt="{alt 或 image}" />
  ```
- **自定义图床域名(可选)**:运行时设置环境变量 `UPLOAD_R2_DOMAIN`,上传接口返回的图片 URL 若为相对路径(`/xxx`)会补全该域名,若主机是默认 `*.r2.dev` 则替换为该域名:
  ```cmd
  set UPLOAD_R2_DOMAIN=https://i.routerpark.com
  wx-mp-sync.exe
  ```

推送的请求(`POST {api_base}/api/bot/sync/articles`):

```json
{
  "title": "文章标题",
  "url": "文章原始链接",
  "content": "Markdown 正文",
  "summary": "摘要",
  "coverImageUrl": "封面图 URL",
  "author": "作者",
  "sourceName": "公众号名",
  "publishedAt": "发布时间(RFC3339)"
}
```

- `title`、`url` 必填(后端契约);`content` 为 Markdown;配置了 `upload_api` 时正文图片会上传图床并替换为 `<CustomImage>` 标签,`coverImageUrl` 取第一张上传后的 URL。
- 保存按钮会显示推送状态:「✓ 已保存·已推送」/「✓ 已保存·推送失败」。

## 命令行批量同步(备选)

```bash
# 直接按 URL 同步(无需微信,无需凭证)
wx-mp-sync.exe sync --url "https://mp.weixin.qq.com/s/xxxx" --output ./articles

# 配置文件 + 定时同步
cp config.example.yaml config.yaml   # 填 URL / 账号凭证
wx-mp-sync.exe watch -c config.yaml --interval 30m
```

## 归档结构

```
articles/
  公众号名/
    2026-08/                          # 按月归档
      文章标题/
        index.html                    # 完整 HTML(图片本地化)
        文章标题.md                   # Markdown
        cover.jpg                     # 封面
        metadata.json                 # 标题/作者/发布时间/原始链接等
        images/                       # 正文图片(URL md5 命名)
  .wx_mp_sync_state.json              # 去重状态(自动维护)
```

## 原理与限制

- **原理**:本地 HTTPS 代理(MITM)拦截 `mp.weixin.qq.com` 的 HTML 响应,注入内联按钮脚本(读取页面 `window.cgiDataNew` 发回本地服务归档)。需要把本地根证书装进 Windows 证书库,微信内置浏览器才会信任拦截证书。
- **前置**:仅支持 **Windows PC 微信**;需要管理员权限装证书/设代理(首次 UAC)。
- **主要风险**:若你的微信版本不走系统代理,按钮可能不出现——此时可改用 `--no-proxy` 手动把微信流量导到代理端口,或后续增加 TUN 虚拟网卡方案。
- 微信对频繁抓取有限流,避免短时间内大量同步。
