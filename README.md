# zlion-music-api

给个人主页用的**音乐直链代理**：只做「搜索 / 取播放直链 / 歌词 / 封面」，**不中转音频流**——
返回的是各平台 CDN 的原始地址，浏览器直接去拉，代理本身不占带宽、也不成为瓶颈。

Go 单二进制、无 CGO、无运行时依赖，风格与 [fileserver](https://github.com/Zlion-Y/fileserver) 一致；
音源脚本可以**在管理页上传或填远程地址自动更新**，改完立即生效、不用重启。

## 为什么需要它

想在自己的主页上用「洛雪音源」那种能力，纯前端做不到两件事：

1. **CORS**——音源脚本要请求各家音乐平台的接口，浏览器里全部被拦；
2. **运行环境**——洛雪自定义音源是 `.js` 脚本，依赖洛雪客户端的宿主 API（`lx.request` / `lx.utils` 等），浏览器里没有这个宿主。

于是把「跑音源脚本」放到服务端：服务端提供一个洛雪脚本宿主，前端只换一个取流地址。

## 取流优先级

`/api/url` 按顺序尝试，任意一环成功即返回：

| 顺序 | 来源 | 说明 |
| --- | --- | --- |
| 1 | **洛雪自定义音源脚本** | `/admin` 上传或填远程地址；脚本声明过的 source 才会路由给它 |
| 2 | **网易官方接口** | 免费曲可用；VIP 曲返回 `url: null`，**这种情况只能靠音源脚本** |

也就是说：**不配脚本也能跑，但只有免费曲**；要拿 VIP / 版权受限曲目的直链，必须配一个音源脚本。

## 下载

不想自己编译就直接下预编译的单文件（无运行时依赖）：

**<https://github.com/Zlion-Y/zlion-music-api/releases>**

| 文件 | 适用 |
| --- | --- |
| `zlion-music-api_linux_amd64` | 常见 x86_64 Linux 服务器 |
| `zlion-music-api_linux_arm64` | ARM64（Armbian 盒子、树莓派 4/5、各类 ARM 小主机） |
| `zlion-music-api_linux_armv7` | 32 位 ARM（老盒子、路由器） |
| `zlion-music-api_windows_amd64.exe` | Windows |
| `zlion-music-api_darwin_arm64` | Apple Silicon macOS |

```bash
chmod +x zlion-music-api_linux_amd64
./zlion-music-api_linux_amd64 -addr 127.0.0.1:8787
```

## 快速开始

```bash
go build -o zlion-music-api .

# 最小启动（网易免费曲）
./zlion-music-api -addr 127.0.0.1:8787

# 带音源脚本（本地文件）
./zlion-music-api -addr 127.0.0.1:8787 -script ./source.js

# 带远程音源脚本：启动即拉取，之后每 6 小时自动检查更新
./zlion-music-api -addr 127.0.0.1:8787 \
  -script ./source.js \
  -script-url https://example.com/lx-source.js \
  -script-interval 6h
```

启动日志里会打印管理页地址和令牌：

```
✅ 音源脚本已加载（启动加载）: ./source.js｜声明音源: wy,kw
🔄 音源脚本自动更新已开启：每 6h0m0s 检查 https://example.com/lx-source.js
🔑 管理页: http://127.0.0.1:8787/admin?token=xxxxxxxx
```

交叉编译（产物在 `dist/`）：

```bash
bash build.sh                    # linux/amd64、linux/arm64、linux/armv7、windows、darwin
bash build.sh linux/amd64        # 只编译指定平台
```

## 音源脚本管理

打开 `/admin`（带令牌），可以直接：

- **上传脚本**：选 `.js` 文件，或把内容粘进文本框 → 应用后立即写盘并热替换；
- **填远程地址**：保存后立刻拉取一次，之后按 `-script-interval` 定时检查，内容变了才热替换；
- 查看当前状态：脚本路径、大小、md5、声明了哪些音源、上次检查时间、上次错误；
- 手动「用磁盘文件重载」。

几条设计上的取舍：

- **磁盘上的脚本文件始终是"生效源"**，上传和远程更新都只是它的写入渠道，所以不会出现"两处配置打架"。
- **热替换失败会保留旧宿主**：一个有语法错误的脚本不会把正在用的音源打哑。
- **上传/远程更新先试跑再落盘**（写临时文件 → 建宿主 → 成功才原子替换），所以坏脚本既不会生效，也不会把磁盘上的好脚本覆盖掉。
- 管理接口由**令牌**保护（`-admin-token`，留空自动生成并写进配置文件），并且**不走 Origin 白名单**——浏览器直接导航打开管理页不带 Origin，走白名单会被自己拦掉。**令牌泄露等于把代码执行权限交出去**，请勿把 `/admin` 暴露给不可信网络（nginx 里最好只允许自己的 IP 或只允许内网访问）。
- 远程地址只接受 `http(s)://`，大小上限 2MB。

配置保存在 `-config`（默认 `zlion-music-api.json`，权限 600）：

```json
{
  "scriptPath": "./source.js",
  "scriptUrl": "https://example.com/lx-source.js",
  "autoUpdate": true,
  "adminToken": "……",
  "updatedAt": "2026-09-14T12:34:42+08:00"
}
```

## 接口

| 接口 | 说明 |
| --- | --- |
| `GET /api/health` | 状态、版本、脚本信息、缓存条目数 |
| `GET /api/search?q=&source=wy&page=1&limit=30` | 搜索（内置网易） |
| `GET /api/url?id=&source=wy&quality=320k` | **取播放直链** |
| `GET /api/url?name=&artist=` | 没有平台 id 时按歌名+歌手交给音源脚本 |
| `GET /api/lyric?id=&source=wy` | 歌词（有脚本时优先脚本，支持逐字歌词字段） |
| `GET /api/pic?id=&source=wy` | 封面 |
| `GET /api/playlist?id=&source=wy` | 网易歌单曲目（自动补全大歌单缺失曲目） |
| `GET /admin` | 音源脚本管理页（需登录）：状态 / 试听测试 / 上传 / 远程更新 / 清缓存 |
| `POST /admin/api/test` | 管理端取流测试（走登录，不受来源白名单限制） |
| `POST /admin/api/cache` | 清空直链与元数据缓存 |

`/api/url` 返回示例：

```json
{
  "ok": true,
  "url": "https://m8.music.126.net/……/xxx.mp3",
  "source": "wy",
  "via": "lx",
  "quality": "320k",
  "name": "屋顶",
  "artist": "周杰伦 / 温岚 / 吴宗宪",
  "expire": 900
}
```

- `via` 表示这一条是谁解析出来的：`lx`=音源脚本、`wy`=网易官方；后缀 `+cache` 表示命中缓存。
- 只传 `id` 时服务会先去网易查歌名/歌手，所以前端**只传网易歌曲 id 就能用**。
- 音质：`128k` / `320k` / `flac` / `flac24bit`，默认取 `-quality`。

## 命令行参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-addr` | `127.0.0.1:8080` | 监听地址，支持 `:8787`、`[::]:446`（IPv6 全接口） |
| `-sources-dir` | `sources` | **音源目录**：里面每个 `.js` 都是一个独立音源 |
| `-script` | `source.js` | 兼容用：单文件模式的音源路径（也会被当作一个音源加载） |
| `-script-url` | 空 | 远程脚本地址，填了即可自动更新 |
| `-script-interval` | `6h` | 自动检查更新的间隔，`0` 表示只手动更新 |
| `-config` | `zlion-music-api.json` | 配置文件（远程地址、管理令牌） |
| `-admin-token` | 自动生成 | 管理接口令牌 |
| `-base-path` | 空 | 挂在子路径下时填前缀（如 `/music`），见下方「反代与子路径」 |
| `-trust-proxy` | false | **前面有反代时必须打开**，否则来源校验形同虚设（见下） |
| `-ui-user` | `admin` | 控制台登录用户名（首页 / 管理页 / 非公开接口） |
| `-ui-pass` | 自动生成 | 控制台登录密码，自动生成时写入配置文件并打进启动日志 |
| `-public-api` | `url,health` | 免登录接口白名单；其余接口一律要登录 |
| `-resolve-concurrency` | 3 | 取流时最多同时问几个音源 |
| `-resolve-hedge` | 300ms | 一波没结果时，再放一波的间隔 |
| `-quarantine` | 10m | 音源连续失败 3 次后的熔断时长，0 表示不熔断 |
| `-verify` | true | **校验音源返回的直链是否真能拉到音频**（关掉快一点，但可能拿到死链） |
| `-verify-timeout` | 5s | 单个直链的校验超时 |
| `-allow` | 见下 | 允许的 Origin（逗号分隔），Referer 主机同样按它校验 |
| `-allow-no-origin` | false | 允许完全不带 Origin/Referer 的请求 |
| `-timeout` | 12s | 上游请求超时 |
| `-ttl` | 15m | 直链缓存时长 |
| `-invoke-timeout` | 20s | 单次音源脚本调用超时 |
| `-quality` | 320k | 默认音质 |
| `-debug` | false | 打印调试日志（含脚本 `updateAlert` 诊断信息） |

## 控制台

界面在 `ui/` 目录里（`index.html` 状态页、`admin.html` 管理页、`style.css`），用 `go:embed` 打进二进制，
改界面不用动 Go 代码、也不影响单文件部署。两个页面都跟随系统深/浅色，手机上自适应。

服务端代码分三个文件：`main.go`（接口与平台对接）、`script.go`（配置与控制台）、`sources.go`（多音源管理）。

### 多音源怎么调度（重要）

不是"把所有音源都打一遍"，那样音源一多就会被上游限流，赢家出现后输的那些还会继续跑到超时。实际调度是：

1. **按成绩排序**：每个音源记录成功率、平均耗时、连续失败次数；成功率高的先问，耗时的靠后。新音源给中性分，不会被埋没；
2. **分波下发**：一波最多同时问 `-resolve-concurrency` 个（默认 3），一波内全失败、或等了 `-resolve-hedge`（默认 300ms）还没结果，就再放一波补位；
3. **赢家一出立刻掐断其余**：拿到第一条可用直链就返回，同时取消这一轮里其它音源的在途 HTTP 请求——连接、goroutine、上游请求量都当场停住；
4. **自动熔断**：连续失败 3 次的音源熔断 `-quarantine`（默认 10 分钟），期间不再打扰它，管理页可以「解除熔断」。竞速输掉被取消的**不算失败**（这点很关键，否则健康音源会被误熔断）；
5. 全部熔断时会临时忽略熔断再试一轮，不会出现"全都熔断=彻底不可用"。

实测效果（21 个真实音源、同一批歌）：全打一遍要 ~3 秒且发 21 个请求；改成这套调度后**单曲取流 50~390ms**，正常只有 3 个并发请求，坏音源熔断后稳定在 50~70ms。

### 多音源与并行体检

音源目录（`-sources-dir`）里可以放**多个** `.js`，每个都是独立音源、各自热加载：

- **取流时并行问所有音源**——凡是启用了、且声明了该平台的脚本会被同时调用，谁先成功用谁。
  单个脚本挂掉或卡住（实测一个忙等 900ms 的脚本）都不会拖慢链路，返回的 `via` 会写明是哪家出的（如 `lx:示例音源`）；
- **并行体检**——管理页点「全部体检」，所有音源同时跑一遍，直接列出谁可用、谁失败、各花了多少毫秒，
  总耗时＝最慢那个（不是相加），可用结果还能当场试听；
- 每个音源可以**单独停用**（不参与取流，文件留着）或**删除**；
- 上传支持**一次选多个文件**（同名即更新该音源，只热替换它）；粘贴上传可以指定文件名；
- **每个音源可以各自配一个远程地址**（文件名 → URL），自动更新会按间隔把配了地址的都检查一遍，内容变了才热替换。

管理页其余功能：查看每个音源的声明平台/大小/md5/加载错误、重新扫描目录、清空缓存、状态 20 秒自动刷新。

## 访问控制（两道门）

**第一道：页面与"前端用不到"的接口，一律要登录。** 首页、`/admin`、以及除 `-public-api` 之外的
所有接口都走 HTTP Basic 授权（账号密码见启动日志，也可用管理令牌）。所以别人扒到你的前端仓库，
只可能看到那个**唯一需要公开**的取流接口，其它什么都进不去。

```bash
-public-api "url,health"    # 默认：只有这两个免登录
-public-api "url"           # 更严格：连 health 都要登录
-public-api "url,health,search,lyric,pic,playlist"   # 全公开（不建议）
```

**第二道：取流接口本身按来源白名单放行。** 前端的地址是公开的（构建产物、CT 日志都能查到），
所以 `/api/url` 只能靠 Origin/Referer 白名单挡住"别人把自己的网页挂上来蹭"和裸脚本抓取。

> ⚠️ **前面有反向代理时一定要加 `-trust-proxy`。** 否则后端看到的连接全来自 `127.0.0.1`（反代自己），
> 会被当成"本机请求"直接放行，白名单就完全失效了——这是实测踩过的坑。
> 开启后按 `X-Forwarded-For` 最右一跳取真实客户端 IP（那一跳由可信反代填写，客户端伪造不了）。

需要清楚的是：白名单用的是请求头，**伪造 `Origin`/`Referer` 仍能绕过**（实测）。它挡的是网页挂载和
无脑抓取，不是铁壁。真要防滥用得靠限流（目前没做，可在反代层加，或给服务加每 IP 速率限制）。

## 访问控制（旧说明，保留）

业务接口只允许白名单来源调用：

- **Origin** 在白名单 → 回 `Access-Control-Allow-Origin`，浏览器才允许跨域读；
- **Referer** 主机在白名单 → 放行（浏览器跨站请求带的是源站 Origin，两个都覆盖到）。

两者都没有时：来自**本机回环**的请求放行（方便 `curl` 自测），其余一律拒绝；想放开就加 `-allow-no-origin`。
`/admin*` 不受此限制，只认令牌。

> 白名单防的是「别人把你的代理挂到自己页面上白嫖」，不是防直接调用——真要防滥用请在反代层加限流。

## IPv6 与端口（重要）

**服务端**没有任何问题：`-addr "[::]:446"` 就能监听所有 IPv6 接口的 446 端口（Linux 下 `[::]` 默认也接 IPv4；443/446 这类低位端口需要 root 或 `setcap`）。

**但前端能不能用，取决于协议，不取决于 IPv6：**

主页是 HTTPS 的，浏览器**禁止** HTTPS 页面请求 `http://` 资源（Mixed Content 拦截），而 IP 字面量不算"可信来源"，localhost 才有豁免。所以：

- ❌ `musicProxy: "http://[2408:xxxx::1]:446"` —— 主页会直接拦掉这个请求（控制台报 Mixed Content）；
- ✅ `musicProxy: "https://music-api.zlion.top:446"` —— 域名 AAAA 指向你的 IPv6、在这个端口上放 TLS 证书即可；
- ⚠️ 直接用 IP 跑 HTTPS 需要证书里带 IP SAN（Let's Encrypt 的 IP 证书目前是短期证书，能用但不方便）。

nginx 上用非标端口 + 域名的写法（`deploy/nginx.conf` 里有完整版）：

```nginx
server {
    listen [::]:446 ssl;
    http2 on;
    server_name music-api.zlion.top;
    ssl_certificate     /etc/letsencrypt/live/music-api.zlion.top/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/music-api.zlion.top/privkey.pem;
    location / {
        proxy_pass http://127.0.0.1:8787;
        # 关键：Origin / Referer 原样透传，否则白名单会误判
        proxy_set_header Origin  $http_origin;
        proxy_set_header Referer $http_referer;
    }
    location /admin { allow 你的管理IP; deny all; }   # 管理页别公开
}
```

另外提醒：访客要能访问你的 IPv6 地址，前提是访客本身有 IPv6 出口——纯 IPv4 的网络下就连不上，这种情况下主页会自动退回 Meting（见下），不会白屏。

## 反代与子路径（Lucky / Nginx / Caddy）

典型部署：程序只监听本机端口，前面挂一层反代（Lucky 的 Web 服务 / 反向代理，或 Nginx、Caddy），
由它负责 DDNS、TLS 和对外域名。

```bash
# 后端：只监听回环；想让反代走 IPv6 就写 -addr "[::]:446"
./zlion-music-api -addr 127.0.0.1:8787 -script /opt/zlion-music-api/source.js
```

反代里把 `https://你的域名` 指向 `http://127.0.0.1:8787` 即可，然后主页配置填**反代后的那个 https 地址**。

两点必须注意：

1. **对外地址必须是 `https://`**。主页是 HTTPS 页面，浏览器会拦掉它去请求 `http://` 资源（Mixed Content），
   IP 字面量不算可信来源、没有 localhost 那种豁免。所以反代这一层要配证书（Lucky 支持自动申请，Nginx/Caddy 同理）；
   直接填 `http://[ipv6]:446` 是调不通的。
2. **Origin / Referer 要原样透传**（默认都会透传，别在反代里覆盖掉），服务的白名单就是靠这两个头判断的。

### 子路径部署

如果反代**不剔除**路径前缀（例如外网是 `https://域名/music/`，转发到后端仍然是 `/music/api/url`），
启动时加 `-base-path /music` 让服务按带前缀注册路由：

```bash
./zlion-music-api -base-path /music ...      # 此时接口在 /music/api/url、管理页在 /music/admin
```

主页里对应填 `musicProxy: "https://你的域名/music"`（不带结尾斜杠）。
反代若能配置「剔除前缀 / strip prefix」，则不需要 `-base-path`，填根地址即可——两种方式选一种，别同时用。

## 音源用不了怎么排查

网上的音源脚本五花八门，失败原因基本就三类。管理页的「音源列表」和「并行体检」能直接区分：

**① 宿主缺东西（本服务的问题，遇到请在 issue 里贴报错）**
脚本会用到洛雪客户端运行时里的一些全局对象，宿主没提供就会 `ReferenceError` 加载失败。本服务已补齐：
`console`（含 group/dir/table/assert/time）、`setTimeout`/`setInterval`、`fetch`、`Buffer`、`btoa`/`atob`、
`TextEncoder`/`TextDecoder`、`process`、`crypto.getRandomValues`、`queueMicrotask`、`structuredClone`，
以及 `lx.currentScriptInfo.rawScript`（有脚本会读自己的源码做校验）。实测一套 21 个音源，补齐前只能加载 11 个，补齐后 20 个。

**② 脚本自己或它的上游挂了（服务改不了）**
典型报错：`获取链接失败，可能是该歌曲无版权或接口维护`、`无可用播放链接`、`Invalid search response`、`Error`。
意思是脚本能跑，但它请求的那个第三方接口挂了/改了/被限流了。只能等作者更新，或者换一个音源。
这类音源**建议直接停用**——虽然并行取流不会因为它失败而变慢，但白问一次也浪费一次请求。

**③ 脚本本身需要特定条件**
比如只按歌名搜索（只给歌曲 id 就报 `Search keyword is empty`）、只支持某些平台、或者要求 VIP cookie。
体检时把「歌名 - 歌手」一起填上，能覆盖更多脚本。

**关于"能返回链接 ≠ 能播"**：不少音源脚本返回的不是真直链，而是它自己那套解析接口的地址（有的甚至不联网、1ms 就返回）。
如果只看"返回了 http 地址就算成功"，这类脚本必然在竞速里胜出；一旦它指向的域名失效，每首歌拿到的都是死链，
而且死链还不计入失败率、坏源永远进不了熔断——**实测踩过这个坑**。

所以服务端默认开启 `-verify`：拿到直链后会真去拉一下（HEAD，不行再 Range 取两个字节），确认状态是 200/206
且 Content-Type 是 `audio/*`（JSON/HTML 一律判失败），**只有验证通过才算这个音源成功**。于是：

- 体检里的「可用」＝**已验证可播**；失败会写明原因，例如 `直链校验不通过：域名解析失败（xxx 不存在）`；
- 假直链会被计入失败率，连续 3 次后自动熔断，不再挡着真音源的路；
- 返回死链时不会立刻失败，而是切换到下一波音源继续找，最终返回的一定是可播的那条。

体检结果里的「试听」按钮依然建议点一下——那是耳朵的最终确认。

## 洛雪音源脚本支持情况

宿主实现了洛雪自定义源 API 的这套面：

- `lx.on(EVENT_NAMES.request, ({source, action, info}) => Promise)`，`action` 支持 `musicUrl` / `lyric` / `pic`
- `lx.send(EVENT_NAMES.inited, {status, sources})`、`updateAlert`（调试日志里能看到）
- `lx.request(url, {method, headers, body, form, formData, timeout, binary}, cb)`，返回取消函数；回调签名 `(err, resp, body)`
- `lx.utils`：`buffer.from` / `buffer.bufToString`（utf8、base64、hex）、`crypto.md5` / `crypto.randomBytes` / `crypto.aesEncrypt` / `crypto.aesDecrypt` / `crypto.rsaEncrypt`、`zlib.inflate` / `zlib.deflate`
- `lx.version` / `lx.env`（`desktop`）/ `lx.currentScriptInfo`

**已知限制：**

- 洛雪**本身不含搜索动作**（搜索是客户端内置的）。所以本服务的搜索只有内置的网易一路，其余平台请用 `/api/url?name=&artist=` 让脚本去解析。
- 只跑**明文 JS 脚本**。被洛雪客户端「加密音源」格式包装过的脚本（整文件是密文、由客户端解密后再执行）不能直接用，需要明文版本。
- `crypto.rsaEncrypt` 传公钥走 PKCS1v15 加密；传私钥时按 SHA256 签名（各家签名算法不一，若你的脚本报错请提 issue）。

`source.example.js` 是一份**能直接跑通**的样板（用网易接口实现 musicUrl / lyric / pic），把里面的接口换成你自己音源脚本用的地址就是一个真正的音源。

## 前端接入（zlion-home）

主页改动只有开关级别，在 `src/config.js` 里改（`musicProxy` 填**反代后拿到的那个 https 地址**）：

```js
musicSource: "proxy",                        // 默认 "meting"
musicProxy: "https://music-api.zlion.top",   // 反代/子路径部署时写 https://域名/music
musicQuality: "320k",
```

接入方式是**并列**而非替换：代理解析出的直链插在候选链第二位，Meting 的候选链一个没删，代理超时/挂掉/返回错误时全部静默退回原有行为。

## 部署

```bash
# 1) 拷贝二进制
scp dist/zlion-music-api_linux_amd64 root@server:/usr/local/bin/zlion-music-api
ssh root@server 'chmod +x /usr/local/bin/zlion-music-api'

# 2) systemd（deploy/zlion-music-api.service，按需改端口 / 脚本路径 / 白名单）
scp deploy/zlion-music-api.service root@server:/etc/systemd/system/
ssh root@server 'systemctl daemon-reload && systemctl enable --now zlion-music-api'

# 3) 反向代理（deploy/nginx.conf 或 deploy/Caddyfile）
#    关键点：Origin / Referer 原样透传；/admin 限制来源

# 4) 打开管理页上传音源脚本（或填远程地址开自动更新）
#    http://<你的地址>:8787/admin?token=<启动日志里的令牌>
```

也可以直接用 Docker（`source.js` 挂进去，配置与脚本目录持久化）：

```bash
docker build -t zlion-music-api .
docker run -d --name zlion-music-api -p 127.0.0.1:8787:8787 \
  -v /opt/zlion-music-api:/data \
  zlion-music-api \
  -addr 0.0.0.0:8787 -script /data/source.js -config /data/config.json \
  -allow "https://www.zlion.top,https://zlion.top"
```

## 免责声明

本服务只是把公开接口的解析逻辑放到自己服务器上跑，**不存储、不中转任何音频内容**，直链来自各平台 CDN。
请自行确认使用场景符合所在地区法律与平台条款，仅供个人学习与自用。
