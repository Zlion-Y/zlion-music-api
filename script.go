package main

// 音源脚本的「生命周期管理」：上传 / 远程 URL / 自动更新 / 热替换 + 一个自带的管理页。
//
// 设计要点：
//   - 磁盘上的脚本文件**始终是生效源**；远程地址只是它的一个更新渠道，
//     上传、远程更新、手动重载最终都汇到 loadScriptFromDisk() 这一个入口。
//   - 热替换失败时保留旧宿主：一个坏脚本不会把正在用的音源打哑。
//   - 管理接口用令牌保护，且**不走 Origin 白名单**——否则从反代域名打开管理页
//     会被自己的白名单拦掉（浏览器直接导航不带 Origin）。

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var scripts *scriptManager

type scriptConfig struct {
	ScriptPath  string            `json:"scriptPath"`
	ScriptURL   string            `json:"scriptUrl,omitempty"` // 旧字段，读取时迁移进 remotes
	AutoUpdate  bool              `json:"autoUpdate"`
	AdminToken  string            `json:"adminToken"`
	ConsoleUser string            `json:"consoleUser"`
	ConsolePass string            `json:"consolePass"`
	Disabled    []string          `json:"disabled,omitempty"` // 停用的音源文件名
	Remotes     map[string]string `json:"remotes,omitempty"`  // 文件名 -> 远程地址（自动更新）
	UpdatedAt   time.Time         `json:"updatedAt"`
}

type scriptManager struct {
	mu       sync.RWMutex
	cfgPath  string
	c        scriptConfig
	interval time.Duration
	lastErr  string
	lastTry  time.Time
	lastOK   time.Time
}

func newScriptManager(cfgPath, scriptPath, scriptURL, token string, auto bool, interval time.Duration) *scriptManager {
	m := &scriptManager{
		cfgPath:  cfgPath,
		interval: interval,
		c: scriptConfig{
			ScriptPath: scriptPath, ScriptURL: scriptURL,
			AutoUpdate: auto, AdminToken: token,
		},
	}
	// 从配置文件恢复的是「远程地址 / 自动更新开关 / 令牌」这类运行时状态；
	// **脚本路径始终以命令行为准**——它是部署参数，改了命令行却因旧配置不生效会非常难查。
	if b, err := os.ReadFile(cfgPath); err == nil {
		var saved scriptConfig
		if json.Unmarshal(b, &saved) == nil {
			if saved.ScriptURL != "" {
				m.c.ScriptURL = saved.ScriptURL
				m.c.AutoUpdate = saved.AutoUpdate
			}
			if saved.AdminToken != "" {
				m.c.AdminToken = saved.AdminToken
			}
			// 控制台账号密码也要恢复，否则每次重启都会重新生成（书签/浏览器记住的密码全废）
			if saved.ConsoleUser != "" {
				m.c.ConsoleUser = saved.ConsoleUser
			}
			if saved.ConsolePass != "" {
				m.c.ConsolePass = saved.ConsolePass
			}
			if len(saved.Disabled) > 0 {
				m.c.Disabled = saved.Disabled
			}
			if len(saved.Remotes) > 0 {
				m.c.Remotes = saved.Remotes
			}
			// 旧的单脚本远程地址迁移成「文件名 -> 地址」
			if saved.ScriptURL != "" && len(saved.Remotes) == 0 {
				m.c.Remotes = map[string]string{filepath.Base(scriptPath): saved.ScriptURL}
			}
		}
	}
	if m.c.AdminToken == "" {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		m.c.AdminToken = hex.EncodeToString(buf)
	}
	return m
}

func (m *scriptManager) save() error {
	m.mu.RLock()
	c := m.c
	m.mu.RUnlock()
	c.UpdatedAt = time.Now()
	if dir := filepath.Dir(m.cfgPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.cfgPath, b, 0o600)
}

// 控制台凭据：命令行给了就用命令行的，否则用配置文件里存的，都没有就生成一个并落盘
func (m *scriptManager) consoleCreds(user, pass string) (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if user == "" {
		user = "admin"
	}
	if pass == "" {
		pass = m.c.ConsolePass
	}
	// 只有"确实没给密码"时才生成：命令行给了就必须用它，否则 -ui-pass 形同虚设
	if pass == "" {
		// 生成一个好念但足够长的随机密码（去掉易混字符）
		const chars = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		for i := range buf {
			buf[i] = chars[int(buf[i])%len(chars)]
		}
		pass = string(buf)
	}
	m.c.ConsoleUser = user
	m.c.ConsolePass = pass
	return user, pass
}

func (m *scriptManager) token() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.c.AdminToken
}

func (m *scriptManager) scriptPath() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.c.ScriptPath
}

func (m *scriptManager) isDisabled(file string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, f := range m.c.Disabled {
		if f == file {
			return true
		}
	}
	return false
}

func (m *scriptManager) setDisabled(file string, disabled bool) {
	m.mu.Lock()
	out := m.c.Disabled[:0]
	for _, f := range m.c.Disabled {
		if f != file {
			out = append(out, f)
		}
	}
	if disabled {
		out = append(out, file)
	}
	m.c.Disabled = out
	m.mu.Unlock()
	_ = m.save()
}

// 每个音源的远程地址（文件名 -> URL），用于自动更新
func (m *scriptManager) remotes() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.c.Remotes))
	for k, v := range m.c.Remotes {
		out[k] = v
	}
	return out
}

func (m *scriptManager) setRemote(file, url string) {
	m.mu.Lock()
	if m.c.Remotes == nil {
		m.c.Remotes = map[string]string{}
	}
	if url == "" {
		delete(m.c.Remotes, file)
	} else {
		m.c.Remotes[file] = url
	}
	m.c.AutoUpdate = len(m.c.Remotes) > 0
	m.mu.Unlock()
	_ = m.save()
}

func (m *scriptManager) scriptURL() string {
	for _, u := range m.remotes() {
		if u != "" {
			return u
		}
	}
	return ""
}

func (m *scriptManager) autoUpdate() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.c.AutoUpdate
}

func (m *scriptManager) markTry(err error) {
	m.mu.Lock()
	m.lastTry = time.Now()
	if err != nil {
		m.lastErr = err.Error()
	}
	m.mu.Unlock()
}

func fileMD5(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func (m *scriptManager) snapshot() map[string]any {
	m.mu.RLock()
	c := m.c
	lastErr, lastTry, lastOK, interval := m.lastErr, m.lastTry, m.lastOK, m.interval
	m.mu.RUnlock()

	list := []map[string]any{}
	for _, e := range sourcesSnapshot() {
		item := map[string]any{
			"file": e.File, "name": e.Name, "sources": e.Sources,
			"enabled": e.Enabled, "loaded": e.Host != nil,
			"size": e.Size, "md5": e.MD5, "remote": c.Remotes[e.File],
		}
		if !e.ModTime.IsZero() {
			item["mtime"] = e.ModTime.Format(time.RFC3339)
		}
		if e.Err != "" {
			item["error"] = e.Err
		}
		list = append(list, item)
	}
	out := map[string]any{
		"sourcesDir": cfg.SourcesDir,
		"list":       list,
		"count":      len(list),
		"enabled":    countEnabled(),
		"autoUpdate": c.AutoUpdate,
		"interval":   interval.String(),
		"lastError":  lastErr,
	}
	if !lastTry.IsZero() {
		out["lastTry"] = lastTry.Format(time.RFC3339)
	}
	if !lastOK.IsZero() {
		out["lastOk"] = lastOK.Format(time.RFC3339)
	}
	return out
}

func countEnabled() int {
	n := 0
	for _, e := range sourcesSnapshot() {
		if e.Enabled && e.Host != nil {
			n++
		}
	}
	return n
}

// 重新扫描音源目录（手动重载 / 远程更新后）
func reloadAllSources() { scanSources() }

// 按远程地址拉取并更新某个音源文件；内容没变就不动
func fetchSourceFromURL(file, u, reason string) (bool, error) {
	if u == "" {
		err := errors.New("未配置远程地址")
		scripts.markTry(err)
		return false, err
	}
	scripts.markTry(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, _, data, err := doReq(ctx, http.MethodGet, u, map[string]string{"User-Agent": uaDesktop}, nil, "")
	if err != nil {
		scripts.markTry(err)
		return false, fmt.Errorf("下载失败: %w", err)
	}
	if code != 200 {
		err := fmt.Errorf("下载失败: HTTP %d", code)
		scripts.markTry(err)
		return false, err
	}
	if len(data) > 2<<20 {
		err := fmt.Errorf("远程脚本过大（%d 字节，上限 2MB）", len(data))
		scripts.markTry(err)
		return false, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		err := errors.New("远程脚本内容为空")
		scripts.markTry(err)
		return false, err
	}
	// 落盘时会 TrimSpace，比较也必须用裁剪后的内容算 md5
	old := entryByFile(file)
	if old != nil {
		sum := md5.Sum(trimmed)
		if old.MD5 == hex.EncodeToString(sum[:]) {
			scripts.mu.Lock()
			scripts.lastErr, scripts.lastOK = "", time.Now()
			scripts.mu.Unlock()
			debugf("远程脚本无变化（%s / %s）", file, reason)
			return false, nil
		}
	}
	if err := writeSourceFile(file, trimmed, reason+"（远程 "+hostOf(u)+"）"); err != nil {
		scripts.markTry(err)
		return false, err
	}
	log.Printf("⬇️  音源已更新（%s）: %s → %s", reason, hostOf(u), file)
	return true, nil
}

// 定时自动更新：把配置了远程地址的音源逐个检查一遍
func (m *scriptManager) loop() {
	if m.interval <= 0 {
		return
	}
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for range t.C {
		if !m.autoUpdate() {
			continue
		}
		for file, u := range m.remotes() {
			if u == "" {
				continue
			}
			if _, err := fetchSourceFromURL(file, u, "自动更新"); err != nil {
				log.Printf("⚠️  音源自动更新失败（%s）: %v", file, err)
			}
		}
	}
}

// ─────────────────────────── 管理接口 ───────────────────────────

// 管理接口鉴权：与首页同一套凭据（Basic 账号密码 或 管理令牌）
func (s *Server) adminAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.consoleOK(r) {
		return true
	}
	fail(w, 401, "需要授权：用启动日志里的控制台账号密码登录，或带上管理令牌")
	return false
}

func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	stats := map[string]any{}
	for _, e := range sourcesSnapshot() {
		stats[e.File] = e.stats.snapshot()
	}
	ok(w, 0, map[string]any{
		"stats":      stats,
		"script":     scripts.snapshot(),
		"configPath": scripts.cfgPath,
		"cache":      map[string]any{"url": urlCache.Len(), "meta": metaCache.Len()},
		"version":    version,
		"uptime":     time.Since(s.start).Round(time.Second).String(),
	})
}

func (s *Server) handleAdminReload(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	scanSources()
	ok(w, 0, map[string]any{"script": scripts.snapshot()})
}

// 删除某个音源
func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	file := sanitizeFileName(firstNonEmpty(r.URL.Query().Get("file"), r.FormValue("file")))
	if file == "" {
		fail(w, 400, "缺少 file 参数")
		return
	}
	if _, err := removeSourceFile(file); err != nil {
		fail(w, 400, "删除失败: %v", err)
		return
	}
	scripts.setRemote(file, "")
	log.Printf("🗑️  音源已删除: %s", file)
	ok(w, 0, map[string]any{"script": scripts.snapshot()})
}

// 启用 / 停用某个音源
func (s *Server) handleAdminToggle(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	q := r.URL.Query()
	file := sanitizeFileName(firstNonEmpty(q.Get("file"), r.FormValue("file")))
	if file == "" {
		fail(w, 400, "缺少 file 参数")
		return
	}
	on := firstNonEmpty(q.Get("enabled"), r.FormValue("enabled")) != "0"
	setSourceEnabled(file, on)
	if on {
		log.Printf("▶️  音源已启用: %s", file)
	} else {
		log.Printf("⏸️  音源已停用: %s", file)
	}
	ok(w, 0, map[string]any{"script": scripts.snapshot()})
}

// 并行体检：把所有音源都问一遍，看哪个还能出流
func (s *Server) handleAdminProbe(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	q := r.URL.Query()
	source := firstNonEmpty(q.Get("source"), "wy")
	id := strings.TrimSpace(q.Get("id"))
	name := strings.TrimSpace(q.Get("name"))
	artist := strings.TrimSpace(q.Get("artist"))
	quality := firstNonEmpty(q.Get("quality"), cfg.Quality)

	matched := ""
	if id == "" && name != "" && source == "wy" {
		if list, _, err := wySearch(r.Context(), strings.TrimSpace(name+" "+artist), 1, 1); err == nil && len(list) > 0 {
			id = list[0].ID
			matched = list[0].Name + " — " + list[0].Artist
		}
	}
	// 与真实取流路径保持一致：只给 id 时也要把歌名/歌手查出来，否则按歌名搜索的音源会直接失败
	if name == "" && id != "" && source == "wy" {
		if n2, a2 := wyMetaCached(r.Context(), id); n2 != "" {
			name, artist = n2, a2
			matched = n2 + " — " + a2
		}
	}
	mi := map[string]any{
		"id": id, "name": name, "singer": artist, "artist": artist,
		"source": source, "quality": quality, "albumName": "",
		"songmid": id, "hash": id, "rid": id, "copyrightId": id,
	}
	t0 := time.Now()
	results := probeAllSources(r.Context(), source, "musicUrl", map[string]any{"type": quality, "musicInfo": mi})
	ok(w, 0, map[string]any{
		"queryId": id, "matched": matched, "quality": quality,
		"elapsed": time.Since(t0).Milliseconds(), "results": results,
	})
}

func (s *Server) handleAdminFetch(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	// 指定 file 就只更新那一个，否则把所有配了远程地址的都过一遍
	only := sanitizeFileName(r.URL.Query().Get("file"))
	changedAny := false
	var firstErr error
	for file, u := range scripts.remotes() {
		if u == "" || (only != "" && file != only) {
			continue
		}
		ch, err := fetchSourceFromURL(file, u, "手动更新")
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if ch {
			changedAny = true
		}
	}
	if firstErr != nil && !changedAny {
		fail(w, 400, "%v", firstErr)
		return
	}
	ok(w, 0, map[string]any{"changed": changedAny, "script": scripts.snapshot()})
}

func (s *Server) handleAdminURL(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	var body struct {
		URL  string `json:"url"`
		File string `json:"file"`
		Auto *bool  `json:"auto"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		body.URL = firstNonEmpty(r.URL.Query().Get("url"), r.FormValue("url"))
		body.File = firstNonEmpty(r.URL.Query().Get("file"), r.FormValue("file"))
		if a := firstNonEmpty(r.URL.Query().Get("auto"), r.FormValue("auto")); a != "" {
			v := a == "1" || a == "true" || a == "on"
			body.Auto = &v
		}
	}
	file := sanitizeFileName(body.File)
	if file == "" {
		file = "remote.js"
	}
	u := strings.TrimSpace(body.URL)
	if u != "" {
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			fail(w, 400, "地址不合法，需要 http(s):// 开头")
			return
		}
	}
	scripts.setRemote(file, u)
	if body.Auto != nil && u != "" && !*body.Auto {
		// 关掉自动更新但保留地址：只清 AutoUpdate，不动 remotes
		scripts.mu.Lock()
		scripts.c.AutoUpdate = false
		scripts.mu.Unlock()
		_ = scripts.save()
	}
	out := map[string]any{"url": u, "file": file}
	if u != "" {
		changed, err := fetchSourceFromURL(file, u, "手动更新")
		if err != nil {
			out["updateError"] = err.Error()
		} else {
			out["changed"] = changed
		}
	}
	out["script"] = scripts.snapshot()
	ok(w, 0, out)
}

func (s *Server) handleAdminUpload(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	type uploaded struct {
		File string `json:"file"`
		Err  string `json:"error,omitempty"`
	}
	var done []uploaded
	fail2 := func(file string, err error) {
		done = append(done, uploaded{File: file, Err: err.Error()})
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			fail(w, 400, "解析上传失败: %v", err)
			return
		}
		// 支持一次传多个文件（字段名统一用 files）
		headers := r.MultipartForm.File["files"]
		if len(headers) == 0 {
			headers = r.MultipartForm.File["file"]
		}
		if len(headers) == 0 {
			fail(w, 400, "没有收到文件（字段名 files）")
			return
		}
		for _, fh := range headers {
			name := sanitizeFileName(fh.Filename)
			if name == "" {
				fail2(fh.Filename, errors.New("文件名不合法"))
				continue
			}
			f, err := fh.Open()
			if err != nil {
				fail2(name, err)
				continue
			}
			data, err := io.ReadAll(io.LimitReader(f, 2<<20))
			f.Close()
			if err != nil {
				fail2(name, err)
				continue
			}
			if err := writeSourceFile(name, data, "手动上传"); err != nil {
				fail2(name, err)
				continue
			}
			done = append(done, uploaded{File: name})
		}
	} else {
		data, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			fail(w, 400, "读取请求体失败: %v", err)
			return
		}
		name := sanitizeFileName(firstNonEmpty(r.URL.Query().Get("name"), r.FormValue("name"), "pasted.js"))
		if name == "" {
			fail(w, 400, "文件名不合法")
			return
		}
		if err := writeSourceFile(name, data, "手动上传"); err != nil {
			fail(w, 400, "应用脚本失败: %v", err)
			return
		}
		done = append(done, uploaded{File: name})
	}

	failed := 0
	for _, d := range done {
		if d.Err != "" {
			failed++
		}
	}
	if failed == len(done) && failed > 0 {
		fail(w, 400, "全部失败: %s", done[0].Err)
		return
	}
	ok(w, 0, map[string]any{"uploaded": done, "failed": failed, "script": scripts.snapshot()})
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTpl.Execute(w, pageData{Base: cfg.BasePath}); err != nil {
		log.Printf("渲染管理页失败: %v", err)
	}
}

// 手动清除某个音源的熔断状态（体检/恢复用）
func (s *Server) handleAdminReset(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	file := sanitizeFileName(firstNonEmpty(r.URL.Query().Get("file"), r.FormValue("file")))
	e := entryByFile(file)
	if e == nil {
		fail(w, 404, "没有这个音源: %s", file)
		return
	}
	e.stats.reset()
	log.Printf("♻️  音源已解除熔断: %s", file)
	ok(w, 0, map[string]any{"stats": e.stats.snapshot()})
}

// 管理端的取流测试：走控制台授权，所以不受来源白名单限制
// （管理页是同源请求，不带 Origin，Referer 又是代理自己的域名，走 /api/url 会被白名单挡掉）
func (s *Server) handleAdminTest(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	q := r.URL.Query()
	t0 := time.Now()
	source := firstNonEmpty(q.Get("source"), "wy")
	id := strings.TrimSpace(q.Get("id"))
	name := strings.TrimSpace(q.Get("name"))
	artist := strings.TrimSpace(q.Get("artist"))
	matched := ""
	// 只给了歌名时，先用网易搜一个 id 出来：音源脚本基本都只认平台 id，
	// 直接拿歌名去问它是问不出结果的
	if id == "" && name != "" && source == "wy" {
		kw := strings.TrimSpace(name + " " + artist)
		if list, _, err := wySearch(r.Context(), kw, 1, 1); err == nil && len(list) > 0 {
			id = list[0].ID
			matched = list[0].Name + " — " + list[0].Artist
		}
	}
	res, err := resolveURL(r.Context(), source, id, name, artist, firstNonEmpty(q.Get("quality"), cfg.Quality))
	if err != nil {
		fail(w, 404, "取流失败: %v", err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "url": res.URL, "source": res.Source, "via": res.Via,
		"quality": res.Quality, "name": res.Name, "artist": res.Artist,
		"expire": res.Expire, "elapsed": time.Since(t0).Milliseconds(),
		"queryId": id, "matched": matched,
	})
}

func (s *Server) handleAdminCache(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(w, r) {
		return
	}
	u, m := urlCache.Flush(), metaCache.Flush()
	ok(w, 0, map[string]any{"cleared": map[string]any{"url": u, "meta": m}})
}
