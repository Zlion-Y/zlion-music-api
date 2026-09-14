// zlion-music-api —— 音乐直链代理（单文件 / 单二进制 / 无 CGO）
//
// 它干什么：
//
//	只做「搜索 / 取播放直链 / 歌词 / 封面」，**不中转音频流**——
//	返回的是各平台 CDN 的原始直链，浏览器直接去拉，代理本身不占带宽。
//
// 为什么要它：
//
//	浏览器直连洛雪音源脚本不可行（CORS + 需要 Node 式宿主环境），
//	所以把「跑音源脚本 + 兜底音源解析」放到服务端跑，前端只换一个取流地址。
//
// 音源优先级（/api/url）：
//  1. 洛雪自定义源脚本（-script 指定 .js）——按 source 路由 musicUrl 动作，最强
//  2. 内置酷我兜底——按「歌名 + 歌手」匹配免费曲，直接出 CDN 直链
//  3. 网易官方接口——免费曲可用，VIP 曲通常返回 null（所以只当最后兜底）
//
// 洛雪自定义源脚本的宿主 API 见 lxHost：lx.on / lx.request / lx.send / lx.utils，
// 脚本只负责 musicUrl / lyric / pic（洛雪本身不含搜索动作，搜索由本服务自己实现）。
package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dop251/goja"
)

// ─────────────────────────── 配置 ───────────────────────────

type Config struct {
	Addr               string        // 监听地址
	ScriptPath         string        // 洛雪自定义源脚本（磁盘上这份始终是"生效源"）
	ScriptURL          string        // 远程脚本地址，填了就能自动/手动更新
	ScriptEvery        time.Duration // 自动更新检查间隔
	ConfigPath         string        // 配置文件（记住远程地址、管理令牌）
	AdminToken         string        // 管理接口令牌
	BasePath           string        // 反代挂在子路径下时用，如 /music（反代不剔除前缀的场景）
	SourcesDir         string        // 音源目录：里面每个 .js 都是一个独立音源
	ResolveConcurrency int           // 取流时最多同时问几个音源
	ResolveHedge       time.Duration // 一波没结果就再放一波的间隔
	Quarantine         time.Duration // 连续失败后的熔断时长
	Verify             bool          // 校验音源返回的直链是否真能拉到音频
	VerifyTimeout      time.Duration // 单个直链的校验超时
	TrustProxy         bool          // 反代在后端前面时打开，用 XFF/X-Real-IP 取真实客户端 IP
	UIUser             string        // 控制台（首页/管理页/非公开接口）的登录用户
	UIPass             string        // 控制台登录密码，留空自动生成并写入配置文件
	PublicAPI          string        // 无需登录即可访问的接口名单，如 "url,health"
	AllowOrigins       []string      // 允许的 Origin / Referer 主机
	AllowNoOrigin      bool          // 允许完全不带 Origin/Referer 的请求（仅本机回环默认放行）
	Timeout            time.Duration // 单次上游请求超时
	TTL                time.Duration // 直链缓存时长
	InvokeTimeout      time.Duration // 单次音源脚本调用超时
	Quality            string        // 默认音质 128k/320k/flac/flac24bit
	Debug              bool
}

var cfg Config

var reBasePath = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)

// 控制台页面：样式与 HTML 放在 ui/ 目录里，编译进二进制（单文件部署不变），
// 以后改界面不用动 Go 代码
//
//go:embed ui
var uiFS embed.FS

var (
	indexTpl = template.Must(template.ParseFS(uiFS, "ui/index.html"))
	adminTpl = template.Must(template.ParseFS(uiFS, "ui/admin.html"))
)

// 页面模板的公共数据
type pageData struct {
	Base string
}

type indexData struct {
	pageData
	Version    string
	Uptime     string
	Healthy    bool
	ScriptPath string
	Sources    string
	PublicAPI  string
	Allow      string
	LastError  string
	URLOpen    bool
	HealthOpen bool
}

// 构建信息（build.sh 用 -ldflags -X 注入）
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func initFlags() {
	var allow string
	flag.StringVar(&cfg.Addr, "addr", "127.0.0.1:8080", "监听地址，如 127.0.0.1:8080 / :8080 / [::]:446（IPv6 全接口）")
	flag.StringVar(&cfg.SourcesDir, "sources-dir", "sources", "音源目录：可放多个 .js 音源，各自独立热加载")
	flag.IntVar(&cfg.ResolveConcurrency, "resolve-concurrency", 3, "取流时最多同时问几个音源（按成绩优先，赢家一出就掐掉其余）")
	flag.DurationVar(&cfg.ResolveHedge, "resolve-hedge", 300*time.Millisecond, "一波音源没结果时，再放一波的间隔")
	flag.DurationVar(&cfg.Quarantine, "quarantine", 10*time.Minute, "音源连续失败 3 次后的熔断时长，期间不再问它；0 表示不熔断")
	flag.BoolVar(&cfg.Verify, "verify", true, "校验音源返回的直链是否真能拉到音频（关掉快一点，但可能拿到死链）")
	flag.DurationVar(&cfg.VerifyTimeout, "verify-timeout", 5*time.Second, "单个直链的校验超时")
	flag.StringVar(&cfg.ScriptPath, "script", "source.js", "兼容用：单文件模式的音源路径（也会被当作一个音源加载）")
	flag.StringVar(&cfg.ScriptURL, "script-url", "", "远程音源脚本地址，填了即可自动更新")
	flag.DurationVar(&cfg.ScriptEvery, "script-interval", 6*time.Hour, "自动检查脚本更新的间隔，0 表示只手动更新")
	flag.StringVar(&cfg.ConfigPath, "config", "zlion-music-api.json", "配置文件路径（保存远程地址、管理令牌）")
	flag.StringVar(&cfg.AdminToken, "admin-token", "", "管理接口令牌，留空自动生成并写入配置文件")
	flag.StringVar(&cfg.BasePath, "base-path", "", "挂在子路径下时填前缀，如 /music（反代不剔除前缀时用；默认根路径）")
	flag.BoolVar(&cfg.TrustProxy, "trust-proxy", false, "前面有反向代理（Lucky/Nginx 等）时打开：按 X-Forwarded-For/X-Real-IP 取真实客户端 IP")
	flag.StringVar(&cfg.UIUser, "ui-user", "admin", "控制台登录用户名")
	flag.StringVar(&cfg.UIPass, "ui-pass", "", "控制台登录密码，留空自动生成并写入配置文件")
	flag.StringVar(&cfg.PublicAPI, "public-api", "url,health", "不需要登录即可访问的接口（逗号分隔）：url/health/search/lyric/pic/playlist；其余一律要登录")
	flag.StringVar(&allow, "allow", "https://www.zlion.top,https://zlion.top,http://localhost:4173,http://localhost:4180,http://localhost:5173,http://127.0.0.1:4173", "允许的 Origin（逗号分隔），Referer 主机同样按此白名单校验")
	flag.BoolVar(&cfg.AllowNoOrigin, "allow-no-origin", false, "允许不带 Origin/Referer 的请求（默认只有本机回环放行；想让 curl/其他机器直接调就打开）")
	flag.DurationVar(&cfg.Timeout, "timeout", 12*time.Second, "上游请求超时")
	flag.DurationVar(&cfg.TTL, "ttl", 15*time.Minute, "直链缓存时长")
	flag.DurationVar(&cfg.InvokeTimeout, "invoke-timeout", 20*time.Second, "单次音源脚本调用超时")
	flag.StringVar(&cfg.Quality, "quality", "320k", "默认音质：128k/320k/flac/flac24bit")
	flag.BoolVar(&cfg.Debug, "debug", false, "打印调试日志")
	flag.Parse()
	// 归一化 base-path：统一成 "/xxx" 形式，去掉结尾斜杠
	cfg.BasePath = strings.TrimSpace(cfg.BasePath)
	if cfg.BasePath != "" && cfg.BasePath != "/" {
		if !strings.HasPrefix(cfg.BasePath, "/") {
			cfg.BasePath = "/" + cfg.BasePath
		}
		cfg.BasePath = strings.TrimRight(cfg.BasePath, "/")
	} else {
		cfg.BasePath = ""
	}
	// 校验一下：ServeMux 遇到含空格等字符的 pattern 会直接 panic，
	// 与其在启动时崩得莫名其妙，不如给一句能看懂的报错
	if cfg.BasePath != "" && !reBasePath.MatchString(cfg.BasePath) {
		log.Fatalf("-base-path 不合法: %q（只允许字母、数字和 - _ . /，且要以 / 开头）", cfg.BasePath)
	}
	for _, o := range strings.Split(allow, ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.AllowOrigins = append(cfg.AllowOrigins, o)
		}
	}
}

func debugf(format string, a ...any) {
	if cfg.Debug {
		log.Printf("[dbg] "+format, a...)
	}
}

// ─────────────────────────── 小工具 ───────────────────────────

const uaDesktop = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

var httpClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
	},
}

// 带超时的上游请求：返回状态码、响应头、响应体
func doReq(ctx context.Context, method, rawurl string, headers map[string]string, body []byte, contentType string) (int, http.Header, []byte, error) {
	cctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(cctx, method, rawurl, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", uaDesktop)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	// 上限 32MB，防止异常响应把内存吃满
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}

func getJSON(ctx context.Context, rawurl string, headers map[string]string, out any) error {
	code, _, data, err := doReq(ctx, http.MethodGet, rawurl, headers, nil, "")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("上游返回 %d", code)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	return nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ─────────────────────────── 内存缓存 ───────────────────────────

type cacheItem struct {
	data []byte
	exp  time.Time
}

type memCache struct {
	mu  sync.Mutex
	m   map[string]cacheItem
	ttl time.Duration
	max int
}

func newCache(ttl time.Duration, max int) *memCache {
	return &memCache{m: make(map[string]cacheItem), ttl: ttl, max: max}
}

func (c *memCache) Get(k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.m[k]
	if !ok {
		return nil, false
	}
	if time.Now().After(it.exp) {
		delete(c.m, k)
		return nil, false
	}
	return it.data, true
}

func (c *memCache) Set(k string, v []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 简单容量控制：超上限就整体清一次（个人代理的量级足够）
	if len(c.m) >= c.max {
		now := time.Now()
		for key, it := range c.m {
			if now.After(it.exp) {
				delete(c.m, key)
			}
		}
		if len(c.m) >= c.max {
			c.m = make(map[string]cacheItem)
		}
	}
	c.m[k] = cacheItem{data: v, exp: time.Now().Add(c.ttl)}
}

// Flush 清空缓存，返回清掉的条数
func (c *memCache) Flush() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.m)
	c.m = make(map[string]cacheItem)
	return n
}

func (c *memCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// ─────────────────────────── 统一歌曲结构 ───────────────────────────

type Song struct {
	ID       string `json:"id"`
	Source   string `json:"source"` // wy=网易 kw=酷我 kg=酷狗 tx=QQ mg=咪咕
	Name     string `json:"name"`
	Artist   string `json:"artist"`
	Album    string `json:"album,omitempty"`
	Duration int    `json:"duration"` // 毫秒
	Pic      string `json:"pic,omitempty"`
}

// ─────────────────────────── 网易（元数据主源：搜索/歌单/歌词/封面） ───────────────────────────

const wyReferer = "https://music.163.com/"

func wyHeaders() map[string]string {
	return map[string]string{
		"Referer": wyReferer,
		"Cookie":  "appver=8.7.01; os=pc",
	}
}

func httpsify(u string) string {
	if strings.HasPrefix(u, "http://") {
		return "https://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

type wySearchResp struct {
	Result struct {
		SongCount int `json:"songCount"`
		Songs     []struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			Duration int    `json:"duration"`
			Fee      int    `json:"fee"`
			Artists  []struct {
				Name string `json:"name"`
			} `json:"artists"`
			Album struct {
				Name   string `json:"name"`
				PicURL string `json:"picUrl"`
			} `json:"album"`
		} `json:"songs"`
	} `json:"result"`
	Code int `json:"code"`
}

func wySearch(ctx context.Context, q string, page, limit int) ([]Song, int, error) {
	offset := (page - 1) * limit
	u := fmt.Sprintf("https://music.163.com/api/search/get/web?s=%s&type=1&limit=%d&offset=%d",
		url.QueryEscape(q), limit, offset)
	var r wySearchResp
	if err := getJSON(ctx, u, wyHeaders(), &r); err != nil {
		return nil, 0, err
	}
	out := make([]Song, 0, len(r.Result.Songs))
	for _, s := range r.Result.Songs {
		names := make([]string, 0, len(s.Artists))
		for _, a := range s.Artists {
			names = append(names, a.Name)
		}
		out = append(out, Song{
			ID:       strconv.FormatInt(s.ID, 10),
			Source:   "wy",
			Name:     s.Name,
			Artist:   strings.Join(names, " / "),
			Album:    s.Album.Name,
			Duration: s.Duration,
			Pic:      httpsify(s.Album.PicURL),
		})
	}
	// 搜索结果里专辑封面常常为空，用批量详情补齐（1 次请求解决整页）
	missing := make([]string, 0, len(out))
	for _, s := range out {
		if s.Pic == "" {
			missing = append(missing, s.ID)
		}
	}
	if len(missing) > 0 {
		if pics, err := wySongsPic(ctx, missing); err == nil {
			for i := range out {
				if p := pics[out[i].ID]; p != "" {
					out[i].Pic = p
				}
			}
		}
	}
	return out, r.Result.SongCount, nil
}

// 批量歌曲详情 → id → 专辑封面
func wySongsPic(ctx context.Context, ids []string) (map[string]string, error) {
	u := "https://music.163.com/api/song/detail?ids=[" + strings.Join(ids, ",") + "]"
	var r struct {
		Songs []struct {
			ID    int64 `json:"id"`
			Album struct {
				PicURL string `json:"picUrl"`
			} `json:"album"`
		} `json:"songs"`
	}
	if err := getJSON(ctx, u, wyHeaders(), &r); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(r.Songs))
	for _, s := range r.Songs {
		m[strconv.FormatInt(s.ID, 10)] = httpsify(s.Album.PicURL)
	}
	return m, nil
}

type wySongDetail struct {
	Songs []struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Duration int    `json:"duration"`
		Artists  []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Album struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			PicURL string `json:"picUrl"`
		} `json:"album"`
	} `json:"songs"`
}

// 单曲详情：拿歌名/歌手，供内置酷我匹配用
func wySong(ctx context.Context, id string) (Song, error) {
	u := "https://music.163.com/api/song/detail?ids=[" + id + "]"
	var r wySongDetail
	if err := getJSON(ctx, u, wyHeaders(), &r); err != nil {
		return Song{}, err
	}
	if len(r.Songs) == 0 {
		return Song{}, fmt.Errorf("网易歌曲 %s 不存在", id)
	}
	s := r.Songs[0]
	names := make([]string, 0, len(s.Artists))
	for _, a := range s.Artists {
		names = append(names, a.Name)
	}
	return Song{
		ID:       id,
		Source:   "wy",
		Name:     s.Name,
		Artist:   strings.Join(names, " / "),
		Album:    s.Album.Name,
		Duration: s.Duration,
		Pic:      httpsify(s.Album.PicURL),
	}, nil
}

// 网易官方播放直链（VIP 曲通常 url=null，所以只作最后兜底）
func wySongURL(ctx context.Context, id, quality string) (string, error) {
	br := 320000
	switch quality {
	case "128k":
		br = 128000
	case "flac":
		br = 999000
	case "flac24bit":
		br = 1900000
	}
	u := fmt.Sprintf("https://music.163.com/api/song/enhance/player/url?id=%s&ids=[%s]&br=%d", id, id, br)
	var r struct {
		Data []struct {
			URL  string `json:"url"`
			Br   int    `json:"br"`
			Code int    `json:"code"`
		} `json:"data"`
	}
	if err := getJSON(ctx, u, wyHeaders(), &r); err != nil {
		return "", err
	}
	if len(r.Data) == 0 || r.Data[0].URL == "" {
		return "", fmt.Errorf("网易未返回可播放直链（多为 VIP/版权限制）")
	}
	return httpsify(r.Data[0].URL), nil
}

func wyLyric(ctx context.Context, id string) (string, string, error) {
	u := fmt.Sprintf("https://music.163.com/api/song/lyric?id=%s&lv=-1&kv=-1&tv=-1", id)
	var r struct {
		Lrc struct {
			Lyric string `json:"lyric"`
		} `json:"lrc"`
		Tlyric struct {
			Lyric string `json:"lyric"`
		} `json:"tlyric"`
	}
	if err := getJSON(ctx, u, wyHeaders(), &r); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(r.Lrc.Lyric) == "" {
		return "", "", fmt.Errorf("网易未返回歌词")
	}
	return r.Lrc.Lyric, r.Tlyric.Lyric, nil
}

func wyPlaylist(ctx context.Context, id string, limit int) (string, []Song, error) {
	if limit <= 0 {
		limit = 1000
	}
	u := fmt.Sprintf("https://music.163.com/api/v6/playlist/detail?id=%s&n=%d&s=0", id, limit)
	var r struct {
		Playlist struct {
			Name       string `json:"name"`
			TrackCount int    `json:"trackCount"`
			Tracks     []struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				Duration int    `json:"dt"`
				Ar       []struct {
					Name string `json:"name"`
				} `json:"ar"`
				Al struct {
					Name   string `json:"name"`
					PicURL string `json:"picUrl"`
				} `json:"al"`
			} `json:"tracks"`
			TrackIDs []struct {
				ID int64 `json:"id"`
			} `json:"trackIds"`
		} `json:"playlist"`
	}
	if err := getJSON(ctx, u, wyHeaders(), &r); err != nil {
		return "", nil, err
	}
	p := r.Playlist
	songs := make([]Song, 0, len(p.Tracks))
	seen := make(map[string]bool)
	for _, t := range p.Tracks {
		names := make([]string, 0, len(t.Ar))
		for _, a := range t.Ar {
			names = append(names, a.Name)
		}
		sid := strconv.FormatInt(t.ID, 10)
		seen[sid] = true
		songs = append(songs, Song{
			ID: sid, Source: "wy", Name: t.Name,
			Artist: strings.Join(names, " / "), Album: t.Al.Name,
			Duration: t.Duration, Pic: httpsify(t.Al.PicURL),
		})
	}
	// 大歌单接口可能只回前若干首：用 trackIds 里缺的 id 批量补详情
	var missing []string
	for _, t := range p.TrackIDs {
		sid := strconv.FormatInt(t.ID, 10)
		if !seen[sid] {
			missing = append(missing, sid)
		}
	}
	if len(missing) > 0 {
		const chunk = 200
		for i := 0; i < len(missing) && i < limit; i += chunk {
			end := i + chunk
			if end > len(missing) {
				end = len(missing)
			}
			part := missing[i:end]
			u := "https://music.163.com/api/song/detail?ids=[" + strings.Join(part, ",") + "]"
			var d wySongDetail
			if err := getJSON(ctx, u, wyHeaders(), &d); err != nil {
				break
			}
			for _, s := range d.Songs {
				names := make([]string, 0, len(s.Artists))
				for _, a := range s.Artists {
					names = append(names, a.Name)
				}
				songs = append(songs, Song{
					ID: strconv.FormatInt(s.ID, 10), Source: "wy", Name: s.Name,
					Artist: strings.Join(names, " / "), Album: s.Album.Name,
					Duration: s.Duration, Pic: httpsify(s.Album.PicURL),
				})
			}
		}
	}
	return p.Name, songs, nil
}

// ─────────────────────────── 洛雪自定义音源脚本宿主 ───────────────────────────

// lxBootstrap 在用户脚本之前注入：把 Promise 风格的处理函数包装成回调风格，
// 这样 Go 侧不需要去内省 goja 的 Promise（goja 顶层调用返回时会自动清空微任务队列）。
const lxBootstrap = `
(function () {
  var handler = null
  globalThis.__lxSetHandler = function (fn) { handler = fn }
  globalThis.__lxHasHandler = function () { return typeof handler === 'function' }
  // 字节数组 → 真 Uint8Array（逐元素拷贝，避免依赖宿主类型）
  globalThis.__lxBytes = function (arr) {
    var out = new Uint8Array(arr.length)
    for (var i = 0; i < arr.length; i++) out[i] = arr[i] & 0xff
    return out
  }
  // ---- 标准运行时的垫片：洛雪客户端里有的全局对象给补上 ----
  // console（warn/error 一定打，其余只在 -debug 时打）
  var mkLog = function (level, always) {
    return function () {
      var parts = []
      for (var i = 0; i < arguments.length; i++) {
        var a = arguments[i]
        try { parts.push(typeof a === 'string' ? a : JSON.stringify(a)) } catch (e) { parts.push(String(a)) }
      }
      try { __lxLog(level, parts.join(' '), always) } catch (e) {}
    }
  }
  var logDepth = 0
  var mkIndent = function (fn) {
    return function () {
      var pre = logDepth > 0 ? new Array(logDepth + 1).join('  ') : ''
      if (pre) { arguments[0] = pre + String(arguments[0]) }
      return fn.apply(null, arguments)
    }
  }
  globalThis.console = {
    log: mkIndent(mkLog('log', false)), info: mkIndent(mkLog('info', false)),
    debug: mkIndent(mkLog('debug', false)), warn: mkIndent(mkLog('warn', true)),
    error: mkIndent(mkLog('error', true)), trace: mkIndent(mkLog('trace', true)),
    dir: mkIndent(mkLog('dir', false)), table: mkIndent(mkLog('table', false)),
    group: function () { logDepth++; return undefined },
    groupCollapsed: function () { logDepth++; return undefined },
    groupEnd: function () { if (logDepth > 0) { logDepth-- } return undefined },
    assert: function (cond) { if (!cond) { mkLog('assert', true).apply(null, Array.prototype.slice.call(arguments, 1)) } },
    count: function () { return undefined }, countReset: function () { return undefined },
    time: function () { return undefined }, timeEnd: function () { return undefined },
    clear: function () { return undefined }
  }
  // 少数脚本会直接用 WebCrypto 或结构化克隆，垫一下
  if (typeof globalThis.crypto === 'undefined') {
    globalThis.crypto = {
      getRandomValues: function (arr) {
        var b = bufFromGo(new Array(arr.length + 1).join('x'), 'utf8')
        if (arr && arr.length) { for (var i = 0; i < arr.length; i++) { arr[i] = b[i % b.length] } }
        return arr
      },
      randomUUID: function () {
        var h = bufToStringGo(bufFromGo(new Array(17).join('r'), 'utf8'), 'hex')
        return h.slice(0, 8) + '-' + h.slice(8, 12) + '-4' + h.slice(13, 16) + '-a' + h.slice(17, 20) + '-' + h.slice(20, 32)
      }
    }
  }
  if (typeof globalThis.queueMicrotask === 'undefined') {
    globalThis.queueMicrotask = function (f) { Promise.resolve().then(f) }
  }
  if (typeof globalThis.structuredClone === 'undefined') {
    globalThis.structuredClone = function (v) { return v == null ? v : JSON.parse(JSON.stringify(v)) }
  }
  // Buffer（Node 风格，内部就是 Uint8Array，只是多一个 toString(encoding)）
  var wrapBuf = function (v) {
    var b = (v instanceof Uint8Array) ? v : new Uint8Array(v || [])
    if (!b.__isBuf) {
      b.__isBuf = true
      b.toString = function (enc) { try { return bufToStringGo(b, enc || 'utf8') } catch (e) { return '' } }
    }
    return b
  }
  globalThis.Buffer = {
    from: function (v, enc) {
      if (typeof v === 'string') return wrapBuf(bufFromGo(v, enc || 'utf8'))
      return wrapBuf(v)
    },
    alloc: function (n) { return wrapBuf(new Uint8Array(n)) },
    isBuffer: function (v) { return !!(v && v.__isBuf) },
    concat: function (list) {
      var len = 0
      for (var i = 0; i < list.length; i++) len += list[i].length
      var out = new Uint8Array(len), off = 0
      for (var j = 0; j < list.length; j++) { out.set(list[j], off); off += list[j].length }
      return wrapBuf(out)
    },
    byteLength: function (s) { return bufFromGo(String(s), 'utf8').length }
  }
  // base64
  globalThis.btoa = function (s) { return bufToStringGo(bufFromGo(String(s), 'utf8'), 'base64') }
  globalThis.atob = function (s) { return bufToStringGo(bufFromGo(String(s), 'base64'), 'utf8') }
  // TextEncoder / TextDecoder
  globalThis.TextEncoder = function () {
    this.encoding = 'utf-8'
    this.encode = function (s) { return wrapBuf(bufFromGo(String(s == null ? '' : s), 'utf8')) }
  }
  globalThis.TextDecoder = function (enc) {
    this.encoding = String(enc || 'utf-8').toLowerCase()
    this.decode = function (b) { return b ? bufToStringGo(b, 'utf8') : '' }
  }
  // process：字段给全一点，混淆脚本常拿 process.argv/env 判断环境，缺字段会读到 undefined.length
  globalThis.process = {
    version: 'v18.0.0', platform: 'linux', arch: 'x64', pid: 1, ppid: 0,
    title: 'node', argv0: 'node', browser: false,
    env: { NODE_ENV: 'production', HOME: '/root', PATH: '/usr/local/bin:/usr/bin:/bin', TMPDIR: '/tmp', LANG: 'zh_CN.UTF-8' },
    argv: ['node', 'source.js'],
    execPath: '/usr/bin/node', execArgv: [],
    versions: { node: '18.0.0', v8: '10.0.0', modules: '108' },
    nextTick: function (f) { Promise.resolve().then(f) },
    cwd: function () { return '/' },
    chdir: function () { return undefined },
    exit: function () { return undefined },
    on: function () { return undefined },
    once: function () { return undefined },
    emit: function () { return false },
    hrtime: function () { var t = Date.now(); return [Math.floor(t / 1000), (t % 1000) * 1e6] },
    memoryUsage: function () { return { rss: 0, heapTotal: 0, heapUsed: 0 } },
    uptime: function () { return 0 }
  }
  if (typeof globalThis.self === 'undefined') { globalThis.self = globalThis }
  // require：给个会明确报错的桩，避免脚本误以为在 Node 里静默拿到 undefined
  if (typeof globalThis.require === 'undefined') {
    globalThis.require = function (name) { throw new Error('本宿主不支持 require(' + name + ')，请用 lx.request / fetch') }
  }
  // fetch：洛雪宿主只给了 lx.request，但不少音源脚本直接写 fetch，这里用 lx.request 包一层
  globalThis.fetch = function (input, init) {
    init = init || {}
    var url = typeof input === 'string' ? input : ((input && input.url) || String(input))
    var opts = { method: String(init.method || 'GET').toUpperCase() }
    if (init.headers) {
      var hs = {}
      if (typeof init.headers.forEach === 'function' && typeof init.headers.get === 'function') {
        init.headers.forEach(function (v, k) { hs[k] = v })
      } else if (Object.prototype.toString.call(init.headers) === '[object Array]') {
        for (var i = 0; i < init.headers.length; i++) hs[init.headers[i][0]] = init.headers[i][1]
      } else {
        for (var k in init.headers) hs[k] = init.headers[k]
      }
      opts.headers = hs
    }
    if (init.body != null) opts.body = String(init.body)
    return new Promise(function (resolve, reject) {
      lx.request(url, opts, function (err, resp, body) {
        if (err) { reject(new Error(String(err))); return }
        var text = body == null ? '' : String(body)
        var hdrs = {}
        if (resp && resp.headers) { for (var k in resp.headers) hdrs[String(k).toLowerCase()] = resp.headers[k] }
        resolve({
          ok: !!(resp && resp.statusCode >= 200 && resp.statusCode < 300),
          status: resp ? resp.statusCode : 0,
          statusText: resp ? resp.statusMessage : '',
          url: url,
          headers: {
            get: function (n) { return hdrs[String(n).toLowerCase()] || null },
            has: function (n) { return hdrs[String(n).toLowerCase()] != null }
          },
          text: function () { return Promise.resolve(text) },
          json: function () {
            try { return Promise.resolve(JSON.parse(text)) } catch (e) { return Promise.reject(e) }
          },
          arrayBuffer: function () { return Promise.resolve(new ArrayBuffer(0)) }
        })
      })
    })
  }
  globalThis.__lxPromise = function (err, val) {
    return err ? Promise.reject(err) : Promise.resolve(val)
  }
  globalThis.__lxInvoke = function (source, action, info, done) {
    if (typeof handler !== 'function') {
      done(new Error('音源脚本未注册 request 处理器'), null)
      return
    }
    try {
      Promise.resolve(handler({ source: source, action: action, info: info }))
        .then(function (r) { done(null, r) }, function (e) { done(e, null) })
    } catch (e) {
      done(e, null)
    }
  }
})()
`

type lxSourceInfo struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type lxHost struct {
	vm     *goja.Runtime
	jobs   chan func()
	mu     sync.Mutex // 串行化音源脚本调用（goja Runtime 非并发安全）
	ready  chan error
	closed chan struct{}

	statMu sync.RWMutex      // 保护下面三个字段：VM 线程写、HTTP 线程读
	names  map[string]string // source key -> 展示名
	hasReq bool
	ver    string

	scriptName string // 日志前缀（文件名）
	roundMu    sync.Mutex
	roundCtx   context.Context // 当前这一轮取流的 context（见 resolve.go）
	rawScript  string          // 脚本源码：部分音源会读 lx.currentScriptInfo.rawScript 做校验/签名
	timerSeq   int64           // setTimeout/setInterval 的 id 计数
	timerMu    sync.Mutex
	timers     map[int64]bool // id -> 是否已取消
}

func newLxHost(path string) (*lxHost, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	h := &lxHost{
		scriptName: filepath.Base(path),
		rawScript:  string(src),
		vm:         goja.New(),
		jobs:       make(chan func(), 128),
		ready:      make(chan error, 1),
		closed:     make(chan struct{}),
		names:      map[string]string{},
	}
	h.vm.SetFieldNameMapper(goja.UncapFieldNameMapper())
	go h.loop()

	// 未处理的 Promise 拒绝只记日志：goja 默认会 panic，不能让音源脚本把服务带走
	h.vm.SetPromiseRejectionTracker(func(p *goja.Promise, op goja.PromiseRejectionOperation) {
		if op == goja.PromiseRejectionReject {
			log.Printf("音源脚本有未处理的 Promise 拒绝: %v", p.Result())
		}
	})
	h.post(func() {
		defer func() {
			if r := recover(); r != nil {
				h.ready <- fmt.Errorf("音源脚本执行 panic: %v", r)
			}
		}()
		if _, err := h.vm.RunString(lxBootstrap); err != nil {
			h.ready <- fmt.Errorf("注入宿主引导失败: %w", err)
			return
		}
		h.injectAPI()
		if _, err := h.vm.RunScript(path, string(src)); err != nil {
			h.ready <- fmt.Errorf("音源脚本执行失败: %s", jsErrDetail(err))
			return
		}
		if v, ok := goja.AssertFunction(h.vm.Get("__lxHasHandler")); ok {
			if r, err := v(goja.Undefined()); err == nil {
				h.statMu.Lock()
				h.hasReq = r.ToBoolean()
				h.statMu.Unlock()
			}
		}
		h.ready <- nil
	})
	if err := <-h.ready; err != nil {
		return nil, err
	}
	return h, nil
}

func (h *lxHost) post(f func()) {
	select {
	case h.jobs <- f:
	case <-h.closed:
	}
}

// 单 goroutine 独占 VM：所有对 VM 的调用都从这里进
func (h *lxHost) loop() {
	for {
		select {
		case job := <-h.jobs:
			job()
		case <-h.closed:
			return
		}
	}
}

// lx.request(url, options, callback) 的 Go 实现：绕开浏览器 CORS，由服务端代发
func (h *lxHost) injectAPI() {
	vm := h.vm
	lx := vm.NewObject()
	lx.Set("version", "2.6.0")
	lx.Set("env", "desktop")

	events := vm.NewObject()
	events.Set("request", "request")
	events.Set("inited", "inited")
	events.Set("updateAlert", "updateAlert")
	lx.Set("EVENT_NAMES", events)

	info := vm.NewObject()
	info.Set("name", "zlion-music-api")
	info.Set("description", "本地宿主：把洛雪自定义音源跑在服务端")
	info.Set("version", "1.0.0")
	info.Set("author", "zlion")
	info.Set("homepage", "")
	info.Set("rawScript", h.rawScript)
	lx.Set("currentScriptInfo", info)

	// on(event, handler)：只关心 request
	lx.Set("on", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		if name == "request" {
			if setter, ok := goja.AssertFunction(vm.Get("__lxSetHandler")); ok {
				_, _ = setter(goja.Undefined(), call.Argument(1))
			}
			h.statMu.Lock()
			h.hasReq = true
			h.statMu.Unlock()
			debugf("音源脚本注册了 request 处理器")
		}
		return goja.Undefined()
	})

	// send(event, data)：inited 里带音源清单
	lx.Set("send", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		data := call.Argument(1)
		if name == "inited" {
			if obj, ok := data.(*goja.Object); ok {
				if s := obj.Get("sources"); s != nil && !goja.IsUndefined(s) && !goja.IsNull(s) {
					if so, ok := s.(*goja.Object); ok {
						h.statMu.Lock()
						for _, k := range so.Keys() {
							if item, ok := so.Get(k).(*goja.Object); ok {
								h.names[k] = item.Get("name").String()
							} else {
								h.names[k] = k
							}
						}
						h.statMu.Unlock()
					}
				}
				if v := obj.Get("version"); v != nil && !goja.IsUndefined(v) {
					h.statMu.Lock()
					h.ver = v.String()
					h.statMu.Unlock()
				}
			}
			debugf("音源脚本 inited")
		} else if cfg.Debug {
			// updateAlert 等事件：脚本常用来回传诊断信息，调试时打出来
			if b, err := json.Marshal(data.Export()); err == nil && len(b) < 2000 {
				log.Printf("[音源脚本 %s] %s", name, string(b))
			}
		}
		return goja.Undefined()
	})

	lx.Set("request", h.jsRequest)
	lx.Set("utils", h.jsUtils())
	h.injectGlobals(vm)
	vm.Set("lx", lx)
	vm.GlobalObject().Set("lx", lx)
	// 一点常用垫片：部分脚本会用 TextDecoder / Buffer 风格的名字
	vm.Set("__lxHostName", "zlion-music-api")
}

func (h *lxHost) jsRequest(call goja.FunctionCall) goja.Value {
	vm := h.vm
	rawurl := call.Argument(0).String()
	optsObj, _ := call.Argument(1).(*goja.Object)
	cb, ok := goja.AssertFunction(call.Argument(2))
	if !ok {
		cb, ok = goja.AssertFunction(call.Argument(1))
		optsObj = nil
	}
	if !ok {
		panic(vm.NewTypeError("lx.request 需要回调函数"))
	}
	method := http.MethodGet
	headers := map[string]string{}
	var body []byte
	contentType := ""
	timeout := 10 * time.Second
	wantBinary := false
	if optsObj != nil {
		if m := optsObj.Get("method"); m != nil && !goja.IsUndefined(m) {
			method = strings.ToUpper(m.String())
		}
		if hs, ok := optsObj.Get("headers").(*goja.Object); ok {
			for _, k := range hs.Keys() {
				headers[k] = hs.Get(k).String()
			}
		}
		if b := optsObj.Get("body"); b != nil && !goja.IsUndefined(b) && !goja.IsNull(b) {
			if s, ok := b.(*goja.Object); ok {
				body = toBytes(s)
			} else {
				body = []byte(b.String())
			}
		}
		if f, ok := optsObj.Get("form").(*goja.Object); ok {
			vals := url.Values{}
			for _, k := range f.Keys() {
				vals.Set(k, f.Get(k).String())
			}
			body = []byte(vals.Encode())
			contentType = "application/x-www-form-urlencoded"
		}
		if fd, ok := optsObj.Get("formData").(*goja.Object); ok {
			var buf bytes.Buffer
			w := multipart.NewWriter(&buf)
			for _, k := range fd.Keys() {
				_ = w.WriteField(k, fd.Get(k).String())
			}
			_ = w.Close()
			body = buf.Bytes()
			contentType = w.FormDataContentType()
		}
		if t := optsObj.Get("timeout"); t != nil && !goja.IsUndefined(t) && !goja.IsNull(t) {
			if ms, ok := t.Export().(int64); ok && ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
		}
		if b := optsObj.Get("binary"); b != nil && b.ToBoolean() {
			wantBinary = true
		}
	}
	if contentType == "" && body != nil {
		contentType = "application/json"
	}

	cancelled := make(chan struct{})
	go func() {
		base := context.Background()
		h.roundMu.Lock()
		if h.roundCtx != nil {
			base = h.roundCtx
		}
		h.roundMu.Unlock()
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		code, respHeaders, data, err := doReq(ctx, method, rawurl, headers, body, contentType)
		select {
		case <-cancelled:
			return
		default:
		}
		// 注意：goja Runtime 只能在 VM 线程上碰，所有 JS 值都在这里构造
		h.post(func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("音源脚本回调 panic: %v", r)
				}
			}()
			var errVal goja.Value = goja.Null()
			var respVal, bodyVal goja.Value = goja.Null(), goja.Null()
			if err != nil {
				errVal = h.vm.ToValue(err.Error())
			} else {
				hdrs := h.vm.NewObject()
				for k, v := range respHeaders {
					hdrs.Set(k, strings.Join(v, ", "))
				}
				ro := h.vm.NewObject()
				ro.Set("statusCode", code)
				ro.Set("statusMessage", http.StatusText(code))
				ro.Set("headers", hdrs)
				text := string(data)
				ro.Set("body", text)
				ro.Set("raw", text)
				respVal = ro
				if wantBinary {
					bodyVal = bytesToAB(h.vm, data)
				} else {
					bodyVal = h.vm.ToValue(text)
				}
			}
			if _, e := cb(goja.Undefined(), errVal, respVal, bodyVal); e != nil {
				log.Printf("音源脚本回调出错: %v", e)
			}
		})
	}()

	return h.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		select {
		case <-cancelled:
		default:
			close(cancelled)
		}
		return goja.Undefined()
	})
}

// 字节 → JS Uint8Array。goja 的 NewArrayBuffer 返回的是 Go 结构体、
// 不能直接当 Value 传，所以由引导脚本里的 __lxBytes 负责包一层。
// 把 goja 的异常展开成「消息 + JS 调用栈」，只给 err.Error() 会丢掉栈，排查音源脚本时很难受
func jsErrDetail(err error) string {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return ex.String()
	}
	return err.Error()
}

func bytesToAB(vm *goja.Runtime, b []byte) goja.Value {
	fn, ok := goja.AssertFunction(vm.Get("__lxBytes"))
	if !ok {
		return vm.ToValue(string(b))
	}
	v, err := fn(goja.Undefined(), vm.ToValue(b))
	if err != nil {
		return vm.ToValue(string(b))
	}
	return v
}

// 把脚本里各种「buffer」形态统一取成字节：Uint8Array / ArrayBuffer / 字符串(base64|hex|utf8)
func toBytes(v goja.Value) []byte {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	switch t := v.Export().(type) {
	case []byte:
		return t
	case goja.ArrayBuffer:
		return t.Bytes()
	case string:
		return []byte(t)
	}
	if o, ok := v.(*goja.Object); ok {
		if ab := o.Get("buffer"); ab != nil {
			if arr, ok := ab.Export().(goja.ArrayBuffer); ok {
				return arr.Bytes()
			}
		}
		// Uint8Array 兜底：逐个取值
		if ln := o.Get("length"); ln != nil && !goja.IsUndefined(ln) {
			n := int(ln.ToInteger())
			out := make([]byte, 0, n)
			for i := 0; i < n; i++ {
				out = append(out, byte(o.Get(strconv.Itoa(i)).ToInteger()))
			}
			return out
		}
	}
	return []byte(v.String())
}

func (h *lxHost) jsUtils() *goja.Object {
	vm := h.vm
	utils := vm.NewObject()

	buf := vm.NewObject()
	buf.Set("from", func(call goja.FunctionCall) goja.Value {
		v := call.Argument(0)
		format := ""
		if f := call.Argument(1); !goja.IsUndefined(f) && !goja.IsNull(f) {
			format = strings.ToLower(f.String())
		}
		if s, ok := v.Export().(string); ok {
			switch format {
			case "base64":
				if b, err := base64.StdEncoding.DecodeString(s); err == nil {
					return bytesToAB(vm, b)
				}
			case "hex":
				if b, err := hex.DecodeString(s); err == nil {
					return bytesToAB(vm, b)
				}
			}
			return bytesToAB(vm, []byte(s))
		}
		return bytesToAB(vm, toBytes(v))
	})
	buf.Set("bufToString", func(call goja.FunctionCall) goja.Value {
		b := toBytes(call.Argument(0))
		format := "utf8"
		if f := call.Argument(1); !goja.IsUndefined(f) && !goja.IsNull(f) {
			format = strings.ToLower(f.String())
		}
		switch format {
		case "base64":
			return vm.ToValue(base64.StdEncoding.EncodeToString(b))
		case "hex":
			return vm.ToValue(hex.EncodeToString(b))
		}
		return vm.ToValue(string(b))
	})
	utils.Set("buffer", buf)

	cryptoObj := vm.NewObject()
	cryptoObj.Set("md5", func(call goja.FunctionCall) goja.Value {
		sum := md5.Sum([]byte(call.Argument(0).String()))
		return vm.ToValue(hex.EncodeToString(sum[:]))
	})
	cryptoObj.Set("randomBytes", func(call goja.FunctionCall) goja.Value {
		n := int(call.Argument(0).ToInteger())
		if n <= 0 {
			n = 16
		}
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return bytesToAB(vm, b)
	})
	aesFn := func(decrypt bool) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			data := toBytes(call.Argument(0))
			mode := call.Argument(1).String()
			key := toBytes(call.Argument(2))
			iv := toBytes(call.Argument(3))
			out, err := aesCrypt(decrypt, data, mode, key, iv)
			if err != nil {
				log.Printf("音源脚本 aes 调用失败: %v", err)
				return goja.Null()
			}
			return bytesToAB(vm, out)
		}
	}
	cryptoObj.Set("aesEncrypt", aesFn(false))
	cryptoObj.Set("aesDecrypt", aesFn(true))
	cryptoObj.Set("rsaEncrypt", func(call goja.FunctionCall) goja.Value {
		data := toBytes(call.Argument(0))
		keyPEM := call.Argument(1).String()
		out, err := rsaEncrypt(data, keyPEM)
		if err != nil {
			log.Printf("音源脚本 rsa 调用失败: %v", err)
			return goja.Null()
		}
		return bytesToAB(vm, out)
	})
	utils.Set("crypto", cryptoObj)

	zlibObj := vm.NewObject()
	zlibObj.Set("inflate", func(call goja.FunctionCall) goja.Value {
		data := toBytes(call.Argument(0))
		r, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return lxPromise(vm, err.Error(), nil)
		}
		defer r.Close()
		out, err := io.ReadAll(io.LimitReader(r, 32<<20))
		if err != nil {
			return lxPromise(vm, err.Error(), nil)
		}
		return lxPromise(vm, "", bytesToAB(vm, out))
	})
	zlibObj.Set("deflate", func(call goja.FunctionCall) goja.Value {
		data := toBytes(call.Argument(0))
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		_, _ = w.Write(data)
		_ = w.Close()
		return lxPromise(vm, "", bytesToAB(vm, buf.Bytes()))
	})
	utils.Set("zlib", zlibObj)

	return utils
}

// 用 JS 侧的 Promise.resolve/reject 造真 Promise：
// goja 的 *Promise 不能直接 ToValue（会被当 Go struct 反射出去）
func lxPromise(vm *goja.Runtime, errMsg string, val goja.Value) goja.Value {
	fn, ok := goja.AssertFunction(vm.Get("__lxPromise"))
	if !ok {
		if errMsg != "" {
			panic(vm.NewGoError(errors.New(errMsg)))
		}
		return val
	}
	arg := goja.Null()
	if errMsg != "" {
		arg = vm.ToValue(errMsg)
	} else if val != nil {
		arg = val
	}
	// __lxPromise(err, val)：err 非空则 reject
	if errMsg == "" {
		v, _ := fn(goja.Undefined(), goja.Null(), arg)
		return v
	}
	v, _ := fn(goja.Undefined(), arg, goja.Null())
	return v
}

// AES：支持 aes-128/192/256 + ecb/cbc，PKCS7 填充
func aesCrypt(decrypt bool, data []byte, mode string, key, iv []byte) ([]byte, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(mode)), "-")
	if len(parts) < 3 {
		return nil, fmt.Errorf("不支持的加密模式 %q", mode)
	}
	kind := parts[len(parts)-1]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	var out []byte
	switch kind {
	case "ecb":
		out = make([]byte, 0, len(data)+aes.BlockSize)
		buf := make([]byte, aes.BlockSize)
		for i := 0; i < len(data); i += aes.BlockSize {
			end := i + aes.BlockSize
			if end > len(data) {
				end = len(data)
			}
			copy(buf, data[i:end])
			for j := end - i; j < aes.BlockSize; j++ {
				buf[j] = 0
			}
			dst := make([]byte, aes.BlockSize)
			if decrypt {
				block.Decrypt(dst, buf)
			} else {
				block.Encrypt(dst, buf)
			}
			out = append(out, dst...)
		}
	case "cbc":
		if len(iv) != aes.BlockSize {
			return nil, fmt.Errorf("CBC 需要 16 字节 IV")
		}
		if len(data)%aes.BlockSize != 0 {
			if decrypt {
				return nil, fmt.Errorf("密文长度不是块大小的整数倍")
			}
			pad := aes.BlockSize - len(data)%aes.BlockSize
			data = append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
		}
		out = make([]byte, len(data))
		if decrypt {
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
		} else {
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
		}
	default:
		return nil, fmt.Errorf("不支持的加密模式 %q", mode)
	}
	if decrypt {
		return pkcs7Unpad(out), nil
	}
	return out, nil
}

func pkcs7Unpad(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	n := int(b[len(b)-1])
	if n <= 0 || n > aes.BlockSize || n > len(b) {
		return b
	}
	return b[:len(b)-n]
}

// RSA：公钥走 PKCS1v15 加密；给的是私钥就按 SHA256 签名（少数音源用签名接口）
func rsaEncrypt(data []byte, keyPEM string) ([]byte, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(keyPEM)))
	if block == nil {
		return nil, errors.New("无法解析 PEM 公钥")
	}
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rp, ok := pub.(*rsa.PublicKey); ok {
			return rsa.EncryptPKCS1v15(rand.Reader, rp, data)
		}
	}
	if pub, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return rsa.EncryptPKCS1v15(rand.Reader, pub, data)
	}
	if pk, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		sum := sha256.Sum256(data)
		return rsa.SignPKCS1v15(rand.Reader, pk, crypto.SHA256, sum[:])
	}
	if pkAny, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if pk, ok := pkAny.(*rsa.PrivateKey); ok {
			sum := sha256.Sum256(data)
			return rsa.SignPKCS1v15(rand.Reader, pk, crypto.SHA256, sum[:])
		}
	}
	return nil, errors.New("不支持的 RSA 密钥类型")
}

// 调用音源脚本的 request 处理器：source=wy/kw/kg/tx/mg，action=musicUrl/lyric/pic
func (h *lxHost) invoke(ctx context.Context, source, action string, info map[string]any) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	// 记下本轮 context：脚本里的 lx.request 会挂在它下面，
	// 这样"别的音源已经赢了"时能立刻掐断这个音源的在途请求
	h.roundMu.Lock()
	h.roundCtx = ctx
	h.roundMu.Unlock()
	defer func() {
		h.roundMu.Lock()
		h.roundCtx = nil
		h.roundMu.Unlock()
	}()

	type result struct {
		v   any
		err error
	}
	ch := make(chan result, 1)

	h.post(func() {
		defer func() {
			if r := recover(); r != nil {
				select {
				case ch <- result{err: fmt.Errorf("音源脚本 panic: %v", r)}:
				default:
				}
			}
		}()
		doneFn := h.vm.ToValue(func(call goja.FunctionCall) goja.Value {
			errVal := call.Argument(0)
			resVal := call.Argument(1)
			var e error
			if errVal != nil && !goja.IsUndefined(errVal) && !goja.IsNull(errVal) {
				e = errors.New(errVal.String())
			}
			var v any
			if resVal != nil && !goja.IsUndefined(resVal) && !goja.IsNull(resVal) {
				v = resVal.Export()
			}
			select {
			case ch <- result{v: v, err: e}:
			default:
			}
			return goja.Undefined()
		})
		fn, ok := goja.AssertFunction(h.vm.Get("__lxInvoke"))
		if !ok {
			ch <- result{err: errors.New("宿主引导缺失")}
			return
		}
		if _, err := fn(goja.Undefined(), h.vm.ToValue(source), h.vm.ToValue(action), h.vm.ToValue(info), doneFn); err != nil {
			ch <- result{err: err}
		}
	})

	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		// 本轮已经有赢家（或调用方取消）：不再等这个音源
		return nil, ctx.Err()
	case <-time.After(cfg.InvokeTimeout):
		return nil, fmt.Errorf("音源脚本调用超时（%s）", cfg.InvokeTimeout)
	}
}

func (h *lxHost) supports(source string) bool {
	h.statMu.RLock()
	defer h.statMu.RUnlock()
	if !h.hasReq {
		return false
	}
	if len(h.names) == 0 {
		return true // 脚本没声明清单就当全支持，交给它自己判断
	}
	_, ok := h.names[source]
	return ok
}

func (h *lxHost) sourceList() []lxSourceInfo {
	h.statMu.RLock()
	defer h.statMu.RUnlock()
	out := make([]lxSourceInfo, 0, len(h.names))
	for k, v := range h.names {
		out = append(out, lxSourceInfo{Key: k, Name: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ─────────────────────────── 取流主流程 ───────────────────────────

var urlCache = newCache(15*time.Minute, 4096)
var metaCache = newCache(30*time.Minute, 4096)

// 音源现在是「一个目录多个脚本」，统一由 sources.go 的 srcList 管理

type urlResult struct {
	URL     string `json:"url"`
	Source  string `json:"source"`
	Via     string `json:"via"` // lx=音源脚本 kw=酷我兜底 wy=网易官方
	Quality string `json:"quality"`
	Name    string `json:"name,omitempty"`
	Artist  string `json:"artist,omitempty"`
	MatchID string `json:"matchId,omitempty"`
	Expire  int    `json:"expire"` // 建议缓存秒数
}

// 解析播放直链：脚本 → 酷我兜底 → 网易官方
func resolveURL(ctx context.Context, source, id, name, artist, quality string) (*urlResult, error) {
	if quality == "" {
		quality = cfg.Quality
	}
	key := fmt.Sprintf("url:%s:%s:%s:%s:%s", source, id, name, artist, quality)
	if b, ok := urlCache.Get(key); ok {
		var r urlResult
		if json.Unmarshal(b, &r) == nil {
			r.Via += "+cache"
			return &r, nil
		}
	}

	// 只给了网易 id 时，先把歌名/歌手取回来（音源脚本大多要这些字段）
	if name == "" && source == "wy" && id != "" {
		if n2, a2 := wyMetaCached(ctx, id); n2 != "" {
			name, artist = n2, a2
		}
	}
	// 反过来只给了歌名时，先搜一个 id 出来——很多音源脚本只认 id，
	// 缺了它就直接报"缺少参数: songId/songmid/id"
	if id == "" && source == "wy" && name != "" {
		if list, _, err := wySearch(ctx, strings.TrimSpace(name+" "+artist), 1, 1); err == nil && len(list) > 0 {
			id = list[0].ID
		}
	}

	var lastErr error

	// 1) 洛雪自定义音源（按 source 路由，最优先）
	if len(pickHosts(source)) > 0 {
		mi := map[string]any{
			"id":          id,
			"name":        name,
			"singer":      artist,
			"artist":      artist,
			"source":      source,
			"quality":     quality,
			"interval":    "01:00",
			"albumName":   "",
			"songmid":     id,
			"hash":        id,
			"copyrightId": id,
			"rid":         id,
		}
		// 酷狗用 hash、QQ 用 songmid、酷我用 rid，脚本按 source 取对应字段
		if strings.HasPrefix(id, "MUSIC_") {
			mi["rid"] = strings.TrimPrefix(id, "MUSIC_")
		}
		v, via, err := resolveWithHedge(ctx, source, "musicUrl", map[string]any{"type": quality, "musicInfo": mi})
		if err != nil {
			lastErr = fmt.Errorf("音源脚本: %w", err)
			debugf("全部音源取流失败: %v", err)
		} else if link, isURL := v.(string); isURL && strings.HasPrefix(link, "http") {
			r := &urlResult{URL: link, Source: source, Via: "lx:" + via, Quality: quality, Name: name, Artist: artist, Expire: int(cfg.TTL.Seconds())}
			if b, err := json.Marshal(r); err == nil {
				urlCache.Set(key, b)
			}
			return r, nil
		} else {
			lastErr = errors.New("音源脚本未返回有效直链")
		}
	}

	// 2) 网易官方（免费曲可用；VIP 曲通常返回 null，需要音源脚本）
	if source == "wy" && id != "" {
		if link, err := wySongURL(ctx, id, quality); err == nil {
			if verr := verifyIfEnabled(ctx, link); verr != nil {
				lastErr = fmt.Errorf("网易直链校验不通过: %w", verr)
				debugf("网易直链校验不通过: %v", verr)
				return nil, lastErr
			}
			r := &urlResult{URL: link, Source: "wy", Via: "wy", Quality: quality, Name: name, Artist: artist, Expire: int(cfg.TTL.Seconds())}
			if b, e := json.Marshal(r); e == nil {
				urlCache.Set(key, b)
			}
			return r, nil
		} else if lastErr == nil {
			lastErr = err
		}
	}

	if lastErr == nil {
		lastErr = errors.New("没有可用的音源（未配置音源脚本时只能解析网易免费曲）")
	}
	return nil, lastErr
}

// 只给了网易歌曲 id 时，把歌名/歌手查出来（带缓存）。
// 音源脚本基本都按歌名搜索或需要 name/singer 字段，缺了它们很多脚本会直接报错。
func wyMetaCached(ctx context.Context, id string) (string, string) {
	if id == "" {
		return "", ""
	}
	key := "wysong:" + id
	var s Song
	if b, ok := metaCache.Get(key); ok {
		_ = json.Unmarshal(b, &s)
	} else if got, err := wySong(ctx, id); err == nil {
		s = got
		if b, err := json.Marshal(s); err == nil {
			metaCache.Set(key, b)
		}
	}
	return s.Name, s.Artist
}

// ─────────────────────────── HTTP 层 ───────────────────────────

type Server struct {
	start time.Time
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, map[string]any{"ok": false, "error": fmt.Sprintf(format, a...)})
}

func ok(w http.ResponseWriter, cacheSec int, v map[string]any) {
	if v == nil {
		v = map[string]any{}
	}
	v["ok"] = true
	if cacheSec > 0 {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheSec))
	}
	writeJSON(w, 200, v)
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	list := []map[string]any{}
	for _, e := range sourcesSnapshot() {
		item := map[string]any{"file": e.File, "name": e.Name, "enabled": e.Enabled, "sources": e.Sources}
		if e.Err != "" {
			item["error"] = e.Err
		}
		list = append(list, item)
	}
	srcInfo := map[string]any{"loaded": countEnabled() > 0, "count": len(list), "enabled": countEnabled(), "list": list}
	ok(w, 0, map[string]any{
		"service": "zlion-music-api",
		"version": version + " (" + commit + " " + buildTime + ")",
		"uptime":  time.Since(s.start).Round(time.Second).String(),
		"quality": cfg.Quality,
		"script":  srcInfo,
		"cache":   map[string]any{"url": urlCache.Len(), "meta": metaCache.Len()},
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		fail(w, 400, "缺少 q 参数")
		return
	}
	source := firstNonEmpty(r.URL.Query().Get("source"), "wy")
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	limit := atoiDefault(r.URL.Query().Get("limit"), 30)
	if limit > 100 {
		limit = 100
	}
	key := fmt.Sprintf("search:%s:%s:%d:%d", source, q, page, limit)
	if b, ok2 := metaCache.Get(key); ok2 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Cache", "hit")
		_, _ = w.Write(b)
		return
	}
	ctx := r.Context()
	var list []Song
	var total int
	var err error
	switch source {
	case "wy":
		list, total, err = wySearch(ctx, q, page, limit)
	default:
		fail(w, 400, "不支持的 source: %s（内置只有网易 wy；洛雪音源脚本不含搜索动作，其余平台请用 /api/url?name=&artist=）", source)
		return
	}
	if err != nil {
		fail(w, 502, "搜索失败: %v", err)
		return
	}
	payload := map[string]any{
		"ok": true, "source": source, "q": q, "page": page, "limit": limit,
		"total": total, "list": list,
	}
	b, _ := json.Marshal(payload)
	metaCache.Set(key, b)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleURL(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source := firstNonEmpty(q.Get("source"), "wy")
	id := strings.TrimSpace(q.Get("id"))
	name := strings.TrimSpace(q.Get("name"))
	artist := strings.TrimSpace(q.Get("artist"))
	quality := firstNonEmpty(q.Get("quality"), cfg.Quality)
	if id == "" && name == "" {
		fail(w, 400, "至少需要 id 或 name 参数")
		return
	}
	res, err := resolveURL(r.Context(), source, id, name, artist, quality)
	if err != nil {
		fail(w, 404, "取流失败: %v", err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "url": res.URL, "source": res.Source, "via": res.Via,
		"quality": res.Quality, "name": res.Name, "artist": res.Artist,
		"matchId": res.MatchID, "expire": res.Expire,
	})
}

func (s *Server) handleLyric(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source := firstNonEmpty(q.Get("source"), "wy")
	id := strings.TrimSpace(q.Get("id"))
	name := strings.TrimSpace(q.Get("name"))
	artist := strings.TrimSpace(q.Get("artist"))
	if id == "" && name == "" {
		fail(w, 400, "至少需要 id 或 name 参数")
		return
	}
	key := fmt.Sprintf("lyric:%s:%s:%s", source, id, name)
	if b, ok2 := metaCache.Get(key); ok2 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Cache", "hit")
		_, _ = w.Write(b)
		return
	}
	ctx := r.Context()

	// 音源脚本优先（能拿到洛雪格式的逐字歌词）
	if len(pickHosts(source)) > 0 {
		v, _, err := resolveWithHedge(ctx, source, "lyric", map[string]any{"musicInfo": map[string]any{
			"id": id, "name": name, "singer": artist, "songmid": id, "hash": id, "rid": id,
		}})
		if err == nil {
			if m, ok := v.(map[string]any); ok {
				payload := map[string]any{"ok": true, "source": source, "via": "lx",
					"lyric": m["lyric"], "tlyric": m["tlyric"], "rlyric": m["rlyric"], "lxlyric": m["lxlyric"]}
				b, _ := json.Marshal(payload)
				metaCache.Set(key, b)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = w.Write(b)
				return
			}
		}
		debugf("音源脚本歌词失败: %v", err)
	}

	if source == "wy" && id != "" {
		lrc, tlrc, err := wyLyric(ctx, id)
		if err != nil {
			fail(w, 404, "歌词获取失败: %v", err)
			return
		}
		payload := map[string]any{"ok": true, "source": source, "via": "wy", "lyric": lrc, "tlyric": tlrc}
		b, _ := json.Marshal(payload)
		metaCache.Set(key, b)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(b)
		return
	}
	fail(w, 404, "没有可用的歌词来源")
}

func (s *Server) handlePic(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source := firstNonEmpty(q.Get("source"), "wy")
	id := strings.TrimSpace(q.Get("id"))
	if id == "" {
		fail(w, 400, "缺少 id 参数")
		return
	}
	if len(pickHosts(source)) > 0 {
		if v, _, err := resolveWithHedge(r.Context(), source, "pic", map[string]any{"musicInfo": map[string]any{"id": id, "songmid": id, "hash": id, "rid": id}}); err == nil {
			if link, isURL := v.(string); isURL && strings.HasPrefix(link, "http") {
				ok(w, 3600, map[string]any{"source": source, "via": "lx", "url": link})
				return
			}
		}
	}
	if source == "wy" {
		song, err := wySong(r.Context(), id)
		if err != nil {
			fail(w, 404, "封面获取失败: %v", err)
			return
		}
		ok(w, 3600, map[string]any{"source": source, "via": "wy", "url": song.Pic})
		return
	}
	fail(w, 404, "没有可用的封面来源")
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	source := firstNonEmpty(q.Get("source"), "wy")
	if id == "" {
		fail(w, 400, "缺少 id 参数")
		return
	}
	if source != "wy" {
		fail(w, 400, "目前只有网易歌单可直接解析（source=wy）")
		return
	}
	limit := atoiDefault(q.Get("limit"), 1000)
	key := fmt.Sprintf("playlist:%s:%d", id, limit)
	if b, hit := metaCache.Get(key); hit {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Cache", "hit")
		_, _ = w.Write(b)
		return
	}
	name, songs, err := wyPlaylist(r.Context(), id, limit)
	if err != nil {
		fail(w, 502, "歌单解析失败: %v", err)
		return
	}
	payload := map[string]any{"ok": true, "id": id, "name": name, "count": len(songs), "list": songs}
	b, _ := json.Marshal(payload)
	metaCache.Set(key, b)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

// 首页：一个极简自述页 + 自检
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// 子路径部署时首页在 /music 与 /music/ 两个形式上都要能打开
	if p := r.URL.Path; p != "/" && p != cfg.BasePath && p != cfg.BasePath+"/" {
		http.NotFound(w, r)
		return
	}
	entries := sourcesSnapshot()
	d := indexData{
		pageData:   pageData{Base: cfg.BasePath},
		Version:    version + " · " + commit + " · " + buildTime,
		Uptime:     time.Since(s.start).Round(time.Second).String(),
		Healthy:    countEnabled() > 0,
		PublicAPI:  cfg.PublicAPI,
		Allow:      strings.Join(cfg.AllowOrigins, "  "),
		URLOpen:    publicAPI("url"),
		HealthOpen: publicAPI("health"),
		ScriptPath: cfg.SourcesDir,
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		flag := "✅"
		if e.HasProblem() {
			flag = "⚠️"
		} else if !e.Enabled {
			flag = "⏸"
		}
		names = append(names, flag+" "+e.Name+"（"+strings.Join(e.Sources, ",")+"）")
	}
	d.Sources = strings.Join(names, "　")
	if st := scripts.snapshot(); st["lastError"] != nil {
		if e, _ := st["lastError"].(string); e != "" {
			d.LastError = e
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTpl.Execute(w, d); err != nil {
		log.Printf("渲染状态页失败: %v", err)
	}
}

// 某个接口是否在免登录白名单里
func publicAPI(name string) bool {
	for _, n := range strings.Split(cfg.PublicAPI, ",") {
		if strings.TrimSpace(n) == name {
			return true
		}
	}
	return false
}

// ─────────────────────────── 访问控制（CORS + Referer 白名单） ───────────────────────────

func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

func (s *Server) originAllowed(origin string) bool {
	o := strings.TrimRight(hostOf(origin), "/")
	for _, a := range cfg.AllowOrigins {
		if strings.EqualFold(strings.TrimRight(hostOf(a), "/"), o) {
			return true
		}
	}
	return false
}

// 真实客户端 IP：前面挂反代时后端看到的永远是反代自己（通常是 127.0.0.1），
// 不取 XFF 就会把所有请求都当成"本机"放行——白名单形同虚设。开了 -trust-proxy 后
// 取 XFF 最右一跳（那一跳是可信反代填的，伪造不了）。
func clientIP(r *http.Request) string {
	if cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// consoleAuth：首页、管理页以及"前端用不到"的接口，一律要求登录（HTTP Basic）。
// 令牌（-admin-token）也能当凭据用，方便脚本/curl 自动化。
func (s *Server) consoleAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.consoleOK(r) {
			next(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/api/") {
			fail(w, 401, "需要控制台授权（用户名密码见启动日志 / 配置文件）")
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="zlion-music-api", charset="UTF-8"`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>401</title>
<body style="font-family:system-ui,sans-serif;max-width:640px;margin:12vh auto;padding:0 20px;line-height:1.7">
<h2>401 需要授权</h2>
<p>这个服务的控制台（首页 / 管理页）不对外公开，请输入账号密码。</p>
<p style="color:#888;font-size:14px">账号密码在服务启动日志里（<code>journalctl -u zlion-music-api | grep 控制台</code>），
也可以直接用管理令牌：<code>?token=xxx</code>。</p></body>`)
	}
}

// consoleOK：Basic 用户名密码 或 管理令牌，任一命中即通过
func (s *Server) consoleOK(r *http.Request) bool {
	if u, p, ok := r.BasicAuth(); ok {
		if subtle.ConstantTimeCompare([]byte(u), []byte(cfg.UIUser)) == 1 &&
			subtle.ConstantTimeCompare([]byte(p), []byte(cfg.UIPass)) == 1 {
			return true
		}
	}
	tok := r.Header.Get("X-Admin-Token")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if tok == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			tok = strings.TrimPrefix(a, "Bearer ")
		}
	}
	return tok != "" && scripts != nil && tok == scripts.token()
}

// guard：只允许白名单来源访问（浏览器看 Origin/Referer，都允许时加 CORS 头）
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		referer := r.Header.Get("Referer")

		if origin != "" {
			if !s.originAllowed(origin) {
				fail(w, 403, "来源 %s 不在白名单内", origin)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if origin == "" {
			switch {
			case referer != "":
				if !s.originAllowed(referer) {
					fail(w, 403, "来源 %s 不在白名单内", hostOf(referer))
					return
				}
			case isLoopback(clientIP(r)), cfg.AllowNoOrigin:
				// 本机调试 / 显式放行
			default:
				fail(w, 403, "缺少 Origin/Referer，已拒绝（可用 -allow-no-origin 放行）")
				return
			}
		}
		next(w, r)
	}
}

// ─────────────────────────── 启动 ───────────────────────────

func main() {
	log.SetFlags(log.LstdFlags)
	initFlags()

	scripts = newScriptManager(cfg.ConfigPath, cfg.ScriptPath, cfg.ScriptURL, cfg.AdminToken, cfg.ScriptURL != "", cfg.ScriptEvery)
	cfg.UIUser, cfg.UIPass = scripts.consoleCreds(cfg.UIUser, cfg.UIPass)
	// 配了远程地址但本地还没有的文件：先拉下来
	for file, u := range scripts.remotes() {
		if u == "" || entryByFile(file) != nil {
			continue
		}
		log.Printf("本地没有 %s，先从远程地址拉取…", file)
		if _, err := fetchSourceFromURL(file, u, "启动拉取"); err != nil {
			log.Printf("⚠️  拉取失败（%s）: %v", file, err)
		}
	}
	scanSources()
	_ = scripts.save()
	if n := len(scripts.remotes()); n > 0 && scripts.autoUpdate() {
		log.Printf("🔄 音源自动更新已开启：每 %s 检查 %d 个远程地址", cfg.ScriptEvery, n)
	}
	log.Printf("🔐 控制台账号: %s / %s   （首页、管理页、非公开接口都需要它）", cfg.UIUser, cfg.UIPass)
	log.Printf("🔑 管理令牌（脚本/curl 用）: %s", scripts.token())
	log.Printf("🌐 免登录接口: %s；其余接口与页面一律要授权", cfg.PublicAPI)
	if !cfg.TrustProxy {
		log.Printf("⚠️  未开启 -trust-proxy：若前面有反向代理，来源白名单会因为「连接都来自 127.0.0.1」而失效")
	}
	log.Printf("🔗 管理页: http://%s/admin", cfg.Addr)
	go scripts.loop()

	srv := &Server{start: time.Now()}
	mux := http.NewServeMux()
	// 子路径部署（反代不剔除前缀）时所有路由统一带前缀；根路径时前缀为空串、行为不变
	at := func(p string) string { return cfg.BasePath + p }

	// 哪些接口免登录（前端要用的那一个）：其余接口和所有页面都要控制台授权
	public := map[string]bool{}
	for _, n := range strings.Split(cfg.PublicAPI, ",") {
		if n = strings.TrimSpace(n); n != "" {
			public[n] = true
		}
	}
	register := func(name string, h http.HandlerFunc) {
		if public[name] {
			mux.HandleFunc(at("/api/"+name), srv.guard(h))
		} else {
			mux.HandleFunc(at("/api/"+name), srv.consoleAuth(h))
		}
	}
	register("health", srv.handleHealth)
	register("search", srv.handleSearch)
	register("url", srv.handleURL)
	register("lyric", srv.handleLyric)
	register("pic", srv.handlePic)
	register("playlist", srv.handlePlaylist)

	// 首页（状态页）本身也不公开
	mux.HandleFunc(at("/"), srv.consoleAuth(srv.handleIndex))

	// 管理接口：令牌保护，不走 Origin 白名单——浏览器直接导航打开管理页不带 Origin，
	// 走白名单会被自己拦掉；这里的安全性由令牌承担。
	mux.HandleFunc(at("/admin"), srv.consoleAuth(srv.handleAdminPage))
	mux.HandleFunc(at("/admin/api/state"), srv.consoleAuth(srv.handleAdminState))
	mux.HandleFunc(at("/admin/api/upload"), srv.consoleAuth(srv.handleAdminUpload))
	mux.HandleFunc(at("/admin/api/url"), srv.consoleAuth(srv.handleAdminURL))
	mux.HandleFunc(at("/admin/api/reload"), srv.consoleAuth(srv.handleAdminReload))
	mux.HandleFunc(at("/admin/api/fetch"), srv.consoleAuth(srv.handleAdminFetch))
	mux.HandleFunc(at("/admin/api/cache"), srv.consoleAuth(srv.handleAdminCache))
	mux.HandleFunc(at("/admin/api/delete"), srv.consoleAuth(srv.handleAdminDelete))
	mux.HandleFunc(at("/admin/api/toggle"), srv.consoleAuth(srv.handleAdminToggle))
	mux.HandleFunc(at("/admin/api/probe"), srv.consoleAuth(srv.handleAdminProbe))
	mux.HandleFunc(at("/admin/api/reset"), srv.consoleAuth(srv.handleAdminReset))
	mux.HandleFunc(at("/admin/api/test"), srv.consoleAuth(srv.handleAdminTest))

	// 样式文件：内容无敏感信息，单独放出去，便于浏览器缓存
	mux.HandleFunc(at("/assets/main.css"), func(w http.ResponseWriter, r *http.Request) {
		b, err := uiFS.ReadFile("ui/style.css")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=600")
		_, _ = w.Write(b)
	})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	go func() {
		log.Printf("zlion-music-api 已启动: http://%s  （音质 %s，直链缓存 %s）", cfg.Addr, cfg.Quality, cfg.TTL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("正在退出…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	for _, e := range sourcesSnapshot() {
		if e.Host != nil {
			close(e.Host.closed)
		}
	}
}
