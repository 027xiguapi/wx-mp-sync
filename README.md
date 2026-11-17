# wx-mp-sync 公众号文章同步插件

用 Go 编写的微信公众号文章同步插件。核心功能:**在微信 PC 客户端浏览公众号文章时,页面右下角出现「⬇ 同步」和「⬇ 下载」按钮**,一键归档或直接下载文章(HTML + Markdown + 图片),并配套一个本地 Web 管理页。另支持把**豆包(doubao.com)、ChatGPT(chatgpt.com)分享的对话**批量导出为 Markdown + 图片 + metadata.json。

实现参考 [`wx_channels_download-main`](wx_channels_download-main):本地起一个 MITM 代理(`ltaoo/echo`,纯 Go,无需 CGO),拦截 `mp.weixin.qq.com` 的页面响应,在 `</body>` 前注入按钮;点按钮后页面把文章数据 POST 到本地管理服务归档。

## 功能

- **微信内一键同步**:打开公众号文章 → 右下角「⬇ 同步」→ 自动归档
- **一键下载 ZIP**:右下角「⬇ 下载」→ 把当前页面的文章 HTML、Markdown 和全部图片打包成 ZIP,直接下载到浏览器默认下载目录
- **搜索页多选批量下载**:在微信「搜一搜」或搜狗微信的结果页,每个结果可直接点选,一次把勾选的多篇文章抓取为 Markdown + 图片并打包成 ZIP
- **豆包分享下载**:粘贴豆包分享链接(或直接在豆包分享页点「⬇ 下载本页」),把对话正文和全部图片导出为 Markdown + 图片 + metadata.json 的 ZIP
- **ChatGPT 分享下载**:粘贴 `chatgpt.com/s/...` 链接,把对话正文、`AsyncImageGroup` 图片导出为 Markdown + 图片 + metadata.json 的 ZIP
- **公众号最新文章**:打开任意一篇文章后,插件会自动捕获该公众号的会话凭证,在管理页一键拉取「最新 N 篇」并批量下载
- **HTTPS 注入**:自动生成并安装本地根证书(MITM),自动设置系统代理,退出时自动恢复
- **双格式归档**:完整 HTML(图片本地化)+ Markdown,另存封面与 `metadata.json`
- **同步文章到奶茶圈**:把归档文章(标题、正文、标签、配图)发布到奶茶圈论坛 `POST /api/posts`,管理页或文章页「⬆ 发帖」均可触发,帖子立即出现在论坛列表和 `/t/:id`
- **同步小红书到奶茶圈**:粘贴小红书笔记链接,或在小红书笔记页右下角点「⬆ 同步到奶茶圈」,把笔记正文和全部图片发布为奶茶圈帖子(复用管理员登录,支持 PoW 验证码)
- **批量下载小红书到本地**:在管理页粘贴一批笔记链接(或使用 `xhs` 命令),归档为 Markdown 正文 + 原图 + `metadata.json`,支持去重、实时进度和 ZIP 打包
- **独立小红书下载器**:双击 `xhs-downloader.exe` 打开原生窗口,粘贴链接即可下载,不依赖代理、证书或奶茶圈配置
- **本地 Web 管理页**:查看已同步文章、手动粘贴链接同步、按多链接批量下载、下载单篇 ZIP、同步单篇到奶茶圈、同步小红书、看运行日志
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
4. 页面右下角出现两个按钮:
   - 绿色「⬇ 同步」:归档到输出目录;
   - 蓝色「⬇ 下载」:把当前文章打包成 ZIP(HTML + Markdown + 图片)直接下载,同时也会归档。
5. 点击后按钮显示「✓ 已保存」/「✓ 已下载」,文章归档到输出目录,浏览器收到 ZIP。
6. 浏览器打开管理页 `http://127.0.0.1:8080` 查看已同步文章、手动同步、下载单篇 ZIP、看日志。

## 搜索页多选批量下载

在微信里打开**「搜一搜」的文章结果页**(或任何列出多篇公众号文章的列表页),插件会自动进入多选模式:

1. 页面底部出现黑色工具条,显示「已选 0 / 共 N 篇」;
2. **点击任意结果卡片即选中**(再点一次取消),选中的卡片有绿色描边,点击不会跳转文章;
3. 也可以点「全选」/「清空」,最多一次选 100 篇;卡片上带日期时还会出现「选最新10篇」;
4. 点「⬇ 批量下载」后,本地程序按链接**逐篇抓取文章**,生成 Markdown + 图片 + HTML,进度实时显示在工具条上;
5. 全部完成后浏览器自动下载一个 ZIP,每篇文章一个目录,另有 `_manifest.json` 记录结果;
6. 点「退出多选」恢复页面原来的点击行为(页面右下角会保留「☑ 多选」入口,可再次进入)。

**搜狗微信搜索**(`weixin.sogou.com/weixin?type=2&query=...`)也已支持:默认注入范围包含 `mp.weixin.qq.com` 和 `weixin.sogou.com`。
搜狗的结果链接是 `/link?url=...` 跳转链接,插件会在**页面内**逐条还原成真实文章链接(搜狗会校验发起搜索的会话,所以必须在页面里解析),再交给本地程序下载;卡片上会显示「结果时间:最新 … / 最旧 …」,并可用「选最新10篇」快速挑出较新的几篇。

如果页面上的结果没有被识别(没有自动出现多选工具条),先看管理页日志里的 `页面诊断`:
`links` 表示识别到的公众号文章链接数(含搜狗跳转链接),`picks` 表示进入多选后的条目数。若 `links=0`,说明该搜索页不在注入范围或结果不是直接链接,可:

- 用 `--inject-match "mp.weixin.qq.com,weixin.sogou.com,其它域名"` 把页面所在域名也纳入注入;
- 或直接把链接粘到管理页的「批量下载(多链接)」里(搜狗跳转链接例外,它需要你的搜索会话,只能在搜狗结果页里多选)。

批量下载默认每篇间隔 900ms,避免触发微信/搜狗限流;可用配置调整:

```yaml
batch_delay_ms: 900   # 批量下载两篇之间的间隔(毫秒)
```

> 说明:服务端按链接重新抓取文章,不依赖微信页面里的 `cgiDataNew`,因此搜索结果里的链接需要带完整参数(微信搜索结果里的链接通常是完整的 `...&key=...&pass_ticket=...`)。若链接被微信判定为需要验证,该篇会以失败原因记录下来,不影响其余文章。

## 豆包分享下载(正文 + 图片)

支持把豆包(`www.doubao.com`)分享出来的对话导出为 **Markdown 正文 + 全部正文图片 + metadata.json**,并打包成 ZIP。两种用法:

1. **管理页批量**:把多个分享链接粘到「豆包分享下载」框里,点「下载为 ZIP」;每行一个链接,一次最多 50 个。
2. **页面按钮**:程序运行期间(系统代理指向本程序),在浏览器打开 `https://www.doubao.com/thread/...`,页面右下角出现橙色「⬇ 下载本页」,点一下直接下载该分享的 ZIP。

实现上优先解析豆包分享页的服务端渲染数据(里面才有图文混排的图集 block),再用分享页自己的快照接口(`/im/message/share/get` 等)兜底;不需要登录 Cookie,也不启动浏览器:

- 保留问答双方的消息和 Markdown 格式,并把消息里的标题降级,避免和文档标题冲突;
- 正文里的图片(搜索配图、AI 生图)会下载到 `images/`,Markdown 改成指向本地图片;
- AI 生图尽量替换成无水印原图模板;
- 重复下载同一分享会跳过(按归档目录 `metadata.json` 里的分享 id 判断);
- 参考来源最多保留 30 条,超出部分在文末注明省略条数。

归档结构:

```text
articles/
  豆包/
    2026-11/
      Expedia API申请流程/
        Expedia API申请流程.md
        metadata.json
        images/
          01-xxxxxxxx.jpg
```

| 接口 | 说明 |
|---|---|
| `POST /api/doubao/batch` | `{"urls":["https://www.doubao.com/thread/..."]}`,返回 `job_id` |
| `GET /api/doubao/status?id=` | 任务进度(`total/done/ok/skipped/failed/items[]`) |
| `GET /api/doubao/zip?id=` | 打包任务里已成功的分享(每个分享一个目录 + `_manifest.json`) |
| `POST /api/doubao/cancel?id=` | 取消任务(只影响尚未开始的条目) |
| `POST /api/doubao/one` | `{"url":"..."}`,直接返回单篇 ZIP(注入按钮使用) |

> 只支持公开分享链接;`c`/`d` 开头的收藏夹、书签分享暂不支持。豆包改版后接口若变化,可能需要跟着调整。

## ChatGPT 分享下载(正文 + 图片)

支持把 ChatGPT 分享页(`https://chatgpt.com/s/...`,以及旧版 `/share/<uuid>`)导出为 **Markdown 正文 + 全部图片 + metadata.json** 并打包成 ZIP:

1. **管理页批量**:把多个链接粘到「ChatGPT 分享下载」框里,点「下载为 ZIP」,一次最多 50 个;
2. **页面按钮**:程序运行期间打开分享页,右下角出现绿色「⬇ 下载本页」,点一下直接下载该分享的 ZIP。

ChatGPT 分享页是 React Router 服务端渲染的,对话数据在
`window.__reactRouterContext.streamController.enqueue(...)` 的 turbo-stream 里。
插件直接解码这段流数据,不需要登录,也不启动浏览器:

- 正文里的 `<AsyncImageGroup .../>` 组件会替换成对应的图片;
- 图片优先取 `images.openai.com` 的 fullsize 地址,下载到 `images/`,Markdown 改成本地相对路径;
- chatgpt.com 通常需要代理:程序启动时会读取**启动前的系统代理**(例如 Clash)作为上级代理,也可以用
  `--upstream-proxy http://127.0.0.1:7897` 显式指定;
- 分享页的 CSP 默认不允许访问本地服务,插件会把本地管理地址加入页面的 `connect-src`;
  如果浏览器仍报 CSP 错误,强制刷新(或清理 chatgpt.com 的站点数据)后再试,注入脚本也会自动退化为直接导航下载;
- 重复下载同一分享会跳过,上次归档不完整时自动补齐。

归档结构和豆包一致:

```text
articles/
  ChatGPT/
    2026-11/
      分享标题/
        分享标题.md
        metadata.json
        images/
          01-xxxxxxxx.jpg
```

| 接口 | 说明 |
|---|---|
| `POST /api/chatgpt/batch` | `{"urls":["https://chatgpt.com/s/..."]}`,返回 `job_id` |
| `GET /api/batch/status?id=` | 任务进度(与公众号批量任务共用) |
| `GET /api/batch/zip?id=` | 打包成功的分享 |
| `POST /api/chatgpt/one` | `{"url":"..."}`,直接返回单篇 ZIP(注入按钮使用) |

## 怎么获取「最新」的公众号文章

先说结论:**搜狗微信搜索不适合找最新文章**,它的问题不是参数没调对,而是索引本身又旧又不全。实测同一个搜索页:

| 关键词 | 搜狗返回文章的发布时间 |
|---|---|
| `机场通道` | 第 1 页 2016–2023 年,翻到第 3 页最新也只有 2024-03 |
| `机场推荐` | 第 1 页 2016–2022 年 |
| `DeepSeek` | 第 1 页全是当天(2026-10-20) |

而搜狗的**公众号搜索**(`type=1`)现在基本空了(返回「暂无…相关的官方认证订阅号」),页面上也不再提供「时间不限/一天/一周」和「按时间排序」控件(`tsn`、`sort` 参数会被重定向回首页)。所以搜狗只适合**补老文章**,拿最新内容要用下面这些方式。

### 为什么微信「搜一搜」页面上没有按钮

微信 PC 的「搜一搜」不是网页:它是客户端内置**本地页面**
`weixin://resourceid/Search/app.html?query=...`,搜索结果由原生接口
`WeixinJSBridge.invoke("getSearchData", ...)` 返回。插件依赖 MITM 代理注入
HTTP(S) 的 HTML 响应,而这个页面既不经过 HTTP、也没有可注入的响应,所以
**在搜一搜结果页上无法加按钮**——这不是配置问题,是机制限制。
(可以对照:从搜索结果点进文章后,页面变成 `mp.weixin.qq.com/s?...`,那上面的
「⬇ 下载」「⬇ 同步」是正常出现的。)

### 推荐做法:公众号最新文章(自动捕获凭证)

不用碰搜一搜,也不用搜狗:

1. 在微信里打开目标公众号的**任意一篇文章**(或它的公众号主页;插件会从请求 URL
   里自动记下 `__biz / uin / key / pass_ticket` 和会话 Cookie);
2. 打开管理页,在「公众号最新文章(自动捕获)」里选中该公众号,点「拉取最新」;
3. 列表按推送时间从新到旧,勾选需要的那几篇 →「下载选中」→ 自动归档为
   Markdown + 图片并打包 ZIP。

凭证就是微信自己的会话,通常几小时内有效;提示「会话已失效」时,回微信重新打开
该号的一篇文章再拉即可。

### 其它入口

1. **公众号历史消息**:在微信里打开公众号 → 查看历史消息。如果该页面是
   `mp.weixin.qq.com/mp/profile_ext?...`,插件会在页面上直接给出多选工具条;
   如果微信版本把它做成了本地页面,就用上面的「最新文章」方式。
2. **微信「搜一搜」/搜索结果**:逐篇点开文章,用文章页右下角的「⬇ 下载」;
   数量多时更适合用「最新文章」。
3. **搜狗**(现状):当"补历史文章"用,配合「选最新10篇」把它索引里相对较新的挑出来。
4. **命令行**:配置 `config.yaml` 的 `accounts`(`biz/uin/key/pass_ticket`),用
   `wx-mp-sync.exe sync` 通过 `getmsg` 拉最新 N 篇(凭证需自己抓包)。

### 批量相关接口

| 接口 | 说明 |
|---|---|
| `POST /api/batch` | `{"urls":["https://mp.weixin.qq.com/s/..."]}` 或 `{"urls":[{"url":"...","title":"..."}]}`,返回 `job_id` |
| `GET /api/batch/status?id=` | 任务进度:`total/done/ok/skipped/failed/items[]` |
| `GET /api/batch/zip?id=` | 打包任务里已成功归档的文章(每篇一个目录 + `_manifest.json`) |
| `POST /api/batch/cancel?id=` | 取消任务(只影响尚未开始的条目) |
| `GET /api/accounts` | 已自动捕获凭证的公众号列表(不返回 key/pass_ticket) |
| `GET /api/accounts/latest?biz=&limit=` | 用捕获的会话拉取该公众号最新文章列表 |

> 批量下载只做本地归档与打包,不会推送到 `bot_sync`(避免一次选中几十篇时刷屏);需要推送请在管理页对单篇点「同步」。

### 相关参数

| 参数 | 说明 |
|---|---|
| `--proxy-port` | MITM 代理端口(默认 2023;端口被占用时自动顺延到下一个可用端口,启动日志会显示实际端口) |
| `--admin-port` | 管理页端口(默认 8080) |
| `--output` | 文章归档目录 |
| `--no-proxy` | 不自动设置系统代理(需手动把代理指向代理端口) |
| `--no-install-cert` | 不自动安装根证书 |
| `--uninstall-cert` | 卸载根证书后退出 |
| `--inject-match` | 注入匹配的域名,多个用逗号分隔(默认 `mp.weixin.qq.com,weixin.sogou.com,www.doubao.com,chatgpt.com,chat.openai.com`,一般无需改) |
| `--upstream-proxy` | 上级代理(如 `http://127.0.0.1:7897`);留空时自动读取启动前的系统代理,用于访问 chatgpt.com 等站点 |

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

## 同步文章到奶茶圈(可选)

把已归档的公众号文章发布到奶茶圈论坛(`POST /api/posts`)。帖子和站内主题共用 `forum_topics`,发布后立刻出现在论坛列表和 `/t/:id`,正文图片以图集形式展示在正文下方。

```yaml
naicha_sync:
  enabled: true
  api_base: http://localhost:3002   # 论坛地址
  token: ""                         # 论坛的 POST_API_TOKEN;留空依次读 NAICHA_POST_TOKEN、POST_API_TOKEN
  category: discussion              # group/rebate/promo/discussion/poll/choose/lottery/notice
  tags: ["公众号", "奶茶"]           # 最多 6 个,每个最多 12 字
  author: ""                        # 文章没有作者时的兜底昵称
  auto: false                       # true: 每次保存/手动同步后自动发帖
  timeout_sec: 60
```

> 如果配置了 `username` / `password`(或 `cookie`),公众号文章的「同步文章」会自动改走新版接口(`/api/upload` + `/api/posts/create`),不再需要 `token`;不配置时仍走旧版 `POST /api/posts`。

用法:

1. 管理页「同步文章到奶茶圈」粘贴文章链接 → 自动归档并发布(已归档的文章会复用);
2. 管理页「已同步文章」列表里点单篇的「同步文章」→ 直接发布已有归档;
3. 启用后,公众号文章页右下角会多出一个「⬆ 发帖」按钮,点一下归档并发布;即使 `auto: false` 也可以单篇发帖(这个按钮只发论坛,不会重复推送 bot);
4. 配置 `auto: true` 后,在微信里点「⬇ 同步」也会顺带发帖,按钮显示「✓ 已保存·已发帖」;发帖失败不影响本地归档,只显示「·发帖失败」。

规则与限制:

- **正文上限 3000 字**。公众号长文会截断为节选,文末附「本文为节选」、公众号名、发布时间和原文链接;原文链接会压掉微信的 `chksm/key/pass_ticket` 等追踪参数。
- 标题自动裁剪到 4–80 字,标签最多 6 个、每个最多 12 字,作者最多 20 字;分类不合法(包括 `coupon`)时回退 `discussion`。
- **图片最多 6 张**:正文里的本地图片通过 multipart 上传(论坛落盘,不依赖微信图床),外链图片保留 URL;归档根目录的封面图会放在图集第一张。单张超过 5MB 的图片会被跳过。
- 同一归档目录默认只发一次,记录在 `articles/.naicha_sync_state.json`;需要重新发布时在接口里传 `"force": true`。
- 论坛按 IP 限流 **每小时 10 篇**,超过会返回 429;批量同步时请留意。

管理页接口:

| 接口 | 说明 |
|---|---|
| `POST /api/naicha/sync` | `{"dir":"公众号/2026-11/文章"}` 发布已有归档;或 `{"url":"https://mp.weixin.qq.com/s/..."}` 先归档再发布;可选 `"force": true` |

返回 `{ok, id, url, title, images, truncated, duplicate, error, dir}`,`url` 就是论坛帖子地址。未启用或发帖失败时 `ok=false`,`error` 给出原因(401 token、429 限流、400 字段校验等)。

> 论坛侧需要允许匿名发帖,或配置 `POST_API_TOKEN` 并在本插件里填同一个 token。`data/uploads/posts` 的落盘与访问由论坛负责,本插件只负责上传。

## 同步小红书到奶茶圈

把小红书的**笔记正文 + 全部配图**发布成奶茶圈帖子。新版奶茶圈没有旧的 `POST /api/posts` token 接口,所以这里复用**管理员登录**:

```text
POST /api/auth/login(用户名/密码,支持自动求解 PoW 验证码)
  → POST /api/upload(每张图片上传到论坛图床,返回 urlPath)
  → POST /api/posts/create(标题、正文、图片 Markdown、分类、标签)
```

配置(`naicha_sync` 里新增的字段):

```yaml
naicha_sync:
  enabled: true
  api_base: http://localhost:3002
  board: discussion        # 发帖分类 boardSlug,以奶茶圈后台分类标识为准
  username: admin          # 奶茶圈管理员用户名/邮箱
  password: "******"       # 管理员密码
  cookie: ""               # 可选:bbs_session 的值或完整 Cookie,填了就不用账号密码
  xhs_enabled: true        # 默认 true
  xhs_tags: ["奶茶"]        # 追加标签(「小红书」会自动加上)
  xhs_cookie: ""           # 可选:抓取小红书页面用的浏览器 Cookie
```

两种用法:

1. **管理页**:「同步小红书到奶茶圈」粘贴笔记链接(`xiaohongshu.com/explore/...`、`/discovery/item/...` 或 `xhslink.com` 短链)→ 点「同步笔记」。
2. **笔记页按钮**:程序运行期间(系统代理指向本程序)在浏览器打开小红书笔记页,右下角出现红色「⬆ 同步到奶茶圈」,点一下即可。这个方式直接读取页面里已登录的 `window.__INITIAL_STATE__`,不受小红书登录墙影响,推荐优先使用。

> 默认注入域名会自动包含 `www.xiaohongshu.com`;如果你用 `--inject-match` 自定义了域名列表,需要手动把 `www.xiaohongshu.com` 加进去。

规则:

- 标题取笔记标题,为空时取正文第一行;正文只保留笔记原文和图片,**不附带来源/原文链接**。
- 正文里的 `#话题[话题]#` 标签标记会自动去掉(不再出现在帖子正文里),对应的标签仍会作为帖子的 `manualTags` 保留。
- 图片默认最多 18 张,先下载到本地再以 multipart 上传到奶茶圈;单张超过 12MB 或下载失败会跳过,正文仍会发布(全部图片都失败时不会发帖)。
- 视频笔记目前同步正文和封面图,不下载视频文件。
- 标签自动加「小红书」,再合并笔记标签和 `xhs_tags`,最多 10 个。
- 同一篇笔记按笔记 ID 去重,记录在 `articles/.naicha_sync_state.json`;需要重发时给接口传 `"force": true`。
- 若奶茶圈开启了**图形验证码 / Turnstile**,程序无法自动登录:请在 `naicha_sync.cookie` 里粘贴浏览器登录后的 `bbs_session`(Cookie 一般 7 天有效),或临时关闭登录验证码。PoW 验证码会自动求解。
- 管理员发帖仍受论坛自身的发帖间隔、敏感词、审核等规则约束;返回里 `pending: true` 表示帖子进入待审核。

管理页接口:

| 接口 | 说明 |
|---|---|
| `POST /api/xhs/sync` | `{"url":"https://www.xiaohongshu.com/explore/..."}`,可选 `"force": true`;注入按钮会额外带 `"note": {...}` 直接提交页面数据 |

返回 `{ok, id, url, title, images, failed_images, pending, note_id, duplicate, error}`,`url` 就是奶茶圈帖子地址(`/post/<id>`)。

## 小红书笔记批量下载到本地

把一批小红书笔记归档为 **Markdown 正文 + 原图 + `metadata.json`**。这个功能和奶茶圈同步相互独立,`naicha_sync.enabled: false` 时也能用。

### 管理页

在「批量下载小红书到本地」里粘贴链接,每行一个;也支持直接粘贴整段分享文本(会自动提取其中的链接)。点「开始下载」后实时显示进度,全部结束后自动打包 ZIP。一次最多 100 篇。

### 命令行

```bash
# 从文件读取链接(每行一个,# 开头的行会被忽略)
wx-mp-sync.exe xhs --file links.txt --output ./articles

# 直接指定链接,可重复 --url
wx-mp-sync.exe xhs --url "https://www.xiaohongshu.com/discovery/item/..." --delay 1200
```

`--delay` 是两篇笔记之间的间隔毫秒数,默认取配置里的 `batch_delay_ms`(900ms);遇到小红书限流时可以调大。

### 归档结构

```text
articles/
  小红书/
    2026-11/
      笔记标题/
        笔记标题.md                   # 标题 + 清洗过话题标记的正文 + 本地图片
        metadata.json                 # note_id/作者/发布时间/标签/原始链接/video_url 等
        images/
          01-xxxxxxxx.jpg
          02-xxxxxxxx.png
```

`metadata.json` 里的 `external_id` 为 `xhs:<noteId>`,管理页文章列表和单篇 ZIP 下载可以直接识别。

### 规则

- 抓取优先使用移动端页面;公开分享链接通常不需要 Cookie,遇到登录墙时可在 `naicha_sync.xhs_cookie` 里配置浏览器 Cookie。
- 正文里的 `#话题[话题]#` 标记会被清理,标签完整保留在 `metadata.json`。
- 同一篇笔记按 `xhs:<noteId>` 去重,重复下载会标记为「已存在」;上次归档不完整(缺 Markdown 或图片数不足)时会自动补齐。
- 图片最多 100 张、单张 12MB;下载失败的图片保留远程地址写入 Markdown,不影响其它内容。
- 视频笔记只保存封面图和 `metadata.json` 里的 `video_url`,不下载视频文件。
- 批量任务复用管理页的 ZIP 打包逻辑,压缩包里每篇笔记一个目录,并附 `_manifest.json`。

## 独立下载器(xhs-downloader.exe)

如果只想下载小红书笔记,不想启动公众号同步、代理和证书,可以直接双击 `xhs-downloader.exe`:

1. 启动后窗口里有一个链接输入框,粘贴一条或多条小红书笔记链接(每行一个,也支持整段分享文本);
2. 选择保存目录(默认 `%USERPROFILE%\Downloads\小红书`,会记住上次选择);
3. 点「开始下载」,窗口里实时显示每篇的进度;重复笔记会自动跳过;
4. 完成后点「打开目录」查看。

归档格式与上面的「小红书笔记批量下载到本地」完全一致。这个程序**完全独立**:

- 不读取 `config.yaml`,不依赖奶茶圈配置;
- 不启动 MITM 代理,不安装证书;
- 不需要 `wx-mp-sync.exe` 正在运行。

### 同步到奶茶圈论坛

窗口里的「同步目标」输入框可以把同一批链接直接发布到任意论坛项目(默认 `http://localhost:3000`),走论坛的机器人接口 `POST /api/bot/posts`:

1. 填写「同步目标」地址和发帖分类(默认 `discussion`,以论坛后台的分类 `boardSlug` 为准)。目标地址可以自定义:支持 `http`/`https`,省略 `http://` 会自动补全,末尾斜杠会被去掉,例如 `localhost:3000`、`192.168.1.10:3000`、`https://bbs.example.com/naicha`;
2. 填写「机器人账号」(论坛后台创建的机器人用户名、昵称或 ID)和 `BOT_API_SECRET`。密钥必须与论坛服务 `.env` 里的 `BOT_API_SECRET` 完全一致,请求时放在 `x-bot-secret` 请求头;
3. 「标签」用逗号分隔,可选,「小红书」会自动带上;
4. 点「同步到论坛」,程序会逐条抓取笔记正文并创建帖子,日志区显示每篇的结果。

说明:

- 论坛服务需要先在 `.env` 里配置 `BOT_API_SECRET` 并重启,否则接口返回 503;同时在论坛后台创建机器人账号(`isBot=true`),否则会提示机器人账号不存在;
- 机器人接口不支持上传图片,配图会以小红书原图链接(Markdown 图片)写入正文;
- 帖子正文保留笔记描述、去掉 `#话题[话题]#` 标记,最多 18 张配图;
- 按 `xhs:<noteId>` 去重,重复点击只会显示「已同步过」;状态文件在 `%APPDATA%\xhs-downloader\naicha_state.json`;
- 论坛设置和保存目录一起保存在 `%APPDATA%\xhs-downloader\config.json`。`BOT_API_SECRET` 是明文保存的,请注意不要把这个文件发给别人;
- 同步前请确保论坛服务已经启动。

构建:

```bash
go build -ldflags="-H windowsgui -s -w" -o xhs-downloader.exe ./cmd/xhs-downloader
```

`cmd/xhs-downloader/rsrc.syso` 已包含 Windows Common Controls v6 manifest(没有它 `walk` 会报 `TTM_ADDTOOL failed` 并立即退出)。修改 `xhs-downloader.manifest` 后,执行 `go generate ./cmd/xhs-downloader` 重新生成即可。

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
  豆包/                               # 豆包分享
    2026-11/
      对话标题/
        对话标题.md                   # Markdown 正文(图片指向 images/)
        metadata.json                 # 标题/分享者/分享时间/原始链接等
        images/                       # 正文图片
  ChatGPT/                            # ChatGPT 分享
    2026-11/
      分享标题/
        分享标题.md
        metadata.json
        images/
  小红书/                             # 小红书笔记(本地批量下载)
    2026-11/
      笔记标题/
        笔记标题.md
        metadata.json
        images/
  .wx_mp_sync_state.json              # 去重状态(自动维护)
  .naicha_sync_state.json             # 已发布到奶茶圈的帖子记录(自动维护)
```

「⬇ 下载」或管理页「下载 ZIP」产出的压缩包就是上面单篇文章目录的完整副本:

```
文章标题.zip
  index.html                          # 完整 HTML(图片本地化)
  文章标题.md                         # Markdown(图片指向 images/)
  cover.jpg                           # 封面
  metadata.json                       # 标题/作者/发布时间/原始链接等
  images/                             # 正文图片
```

「批量下载」产出的 ZIP 每篇文章一个目录,并附带一份清单:

```
wx-mp-batch-20261020-093000.zip
  文章标题一/
    index.html                        # 完整 HTML(图片本地化)
    文章标题一.md                     # Markdown(图片指向 images/)
    metadata.json
    images/
  文章标题二/
    ...
  _manifest.json                      # 每篇的 url / 标题 / 目录 / 成功或失败原因
```

## 原理与限制

- **原理**:本地 HTTPS 代理(MITM)拦截 `mp.weixin.qq.com` 的 HTML 响应,注入内联脚本:文章页读取 `window.cgiDataNew` 发回本地服务归档;搜索结果页扫描页面上的文章链接,提供多选与批量下载(本地服务按链接逐篇抓取)。需要把本地根证书装进 Windows 证书库,微信内置浏览器才会信任拦截证书。
- **豆包**:解析分享页的服务端渲染数据(含图文混排图集),快照接口兜底,无需登录、无需浏览器渲染;只支持公开分享,页面或接口改版后可能失效。
- **ChatGPT**:解码分享页的 turbo-stream 数据,提取消息与 `AsyncImageGroup` 图片;需要能访问 chatgpt.com(通常经系统代理/Clash),页面改版后流数据格式可能变化。
- **小红书**:解析笔记页的 `window.__INITIAL_STATE__`;默认使用移动端页面(桌面页面容易被重定向到登录页),支持完整链接和 `xhslink.com` 短链,公开分享链接通常无需登录。
- **前置**:仅支持 **Windows PC 微信**;需要管理员权限装证书/设代理(首次 UAC)。
- **主要风险**:若你的微信版本不走系统代理,按钮可能不出现——此时可改用 `--no-proxy` 手动把微信流量导到代理端口,或后续增加 TUN 虚拟网卡方案。
- 微信对频繁抓取有限流,批量下载已内置间隔(默认 900ms),避免短时间内大量同步。

## 常见问题:按钮不出现

1. **先看启动日志里的实际端口**。程序只监听 `127.0.0.1`,不会用通配地址;若配置的端口被其它程序(VS Code、Clash 等)占用,会自动顺延(如 `2024` → `2025`),并打印:
   ```text
   ⚠️ 代理端口 2024 已被占用,已自动改用 2025(系统代理也会指向该端口)
   ```
   若不想自动顺延,可显式换端口:`wx-mp-sync.exe --proxy-port 20333`。
2. **确认 Windows 系统代理指向实际端口**:`设置 → 网络和 Internet → 代理`,应为 `127.0.0.1:<实际端口>`。
3. **重启微信内置浏览器页面**:在微信里关闭文章窗口后重新打开(或右键刷新),旧页面不会重新注入。
4. **代理被其它软件改回**:Clash/公司 VPN 等可能重置系统代理,关闭其“系统代理”开关或退出后重试。
5. 仍不行时用命令行验证代理是否生效(应返回包含 `wx-mp-sync-btn` 的 HTML):
   ```bash
   curl -k -x http://127.0.0.1:2025 https://mp.weixin.qq.com/
   ```
6. **搜索页没有出现多选工具条**:看管理页日志里的「页面诊断」,`links` 字段是识别到的文章链接数。为 0 说明结果页域名不在注入范围或结果不是直接链接,可用 `--inject-match` 增加域名,或直接把链接粘到管理页批量下载。
7. **豆包分享页没有「⬇ 下载本页」**:确认页面是 `https://www.doubao.com/thread/...`、系统代理确实指向本程序,并且 `--inject-match` 里包含 `www.doubao.com`(默认包含)。仍不行时,直接把链接粘到管理页「豆包分享下载」里下载。
8. **ChatGPT 下载失败/超时**:确认本机能访问 chatgpt.com(通常需要 Clash 等代理)。程序启动时会自动把原有系统代理作为上级代理;也可以在启动时加 `--upstream-proxy http://127.0.0.1:7897`。分享页右下角没有按钮时,直接把 `https://chatgpt.com/s/...` 粘到管理页「ChatGPT 分享下载」。
