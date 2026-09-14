package main

// 多音源管理：一个目录里可以放多个洛雪音源脚本，各自独立热加载。
//
//   - 取流（musicUrl）时**并行**问所有声明了该 source 的脚本，谁先成功用谁
//     —— 单个脚本挂掉/超时不再拖慢整条链路；
//   - 管理页可以一键对全部音源做「并行体检」，直接看出哪个还能用；
//   - 每个脚本的文件名就是它的标识，上传同名文件＝更新该脚本（会热替换，不影响其它）。

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type scriptEntry struct {
	File     string // 文件名（相对音源目录；单文件模式下是给用户看的名字）
	Path     string // 实际路径
	stats    srcStats
	Name     string // 展示名：脚本头部的 @name，没有就用文件名
	Sources  []string
	Host     *lxHost
	Enabled  bool
	Size     int64
	MD5      string
	ModTime  time.Time
	Err      string // 加载失败原因（该脚本会被跳过，但不影响其它脚本）
	LoadedAt time.Time
}

var (
	srcMu   sync.RWMutex
	srcList []*scriptEntry
)

func (e *scriptEntry) HasProblem() bool { return e.Err != "" || e.Host == nil }

func sourcesSnapshot() []*scriptEntry {
	srcMu.RLock()
	defer srcMu.RUnlock()
	out := make([]*scriptEntry, len(srcList))
	copy(out, srcList)
	return out
}

// 取所有启用、且声明了该 source 的脚本（脚本没声明清单时视为全支持）
func pickHosts(source string) []*scriptEntry {
	srcMu.RLock()
	defer srcMu.RUnlock()
	out := make([]*scriptEntry, 0, len(srcList))
	for _, e := range srcList {
		if e.Enabled && e.Host != nil && e.Host.supports(source) {
			out = append(out, e)
		}
	}
	return out
}

func firstHost() *lxHost {
	srcMu.RLock()
	defer srcMu.RUnlock()
	for _, e := range srcList {
		if e.Enabled && e.Host != nil {
			return e.Host
		}
	}
	return nil
}

var reScriptName = regexp.MustCompile(`(?m)^\s*\*?\s*@name\s+(.+?)\s*$`)

func scriptDisplayName(path string, src []byte) string {
	if m := reScriptName.FindSubmatch(src); len(m) > 1 {
		if n := strings.TrimSpace(string(m[1])); n != "" {
			return n
		}
	}
	return filepath.Base(path)
}

// 建一个条目（不改全局状态）
func buildEntry(path, file string, enabled bool) *scriptEntry {
	e := &scriptEntry{File: file, Path: path, Enabled: enabled, Name: file}
	st, err := os.Stat(path)
	if err != nil {
		e.Err = "读取失败: " + err.Error()
		return e
	}
	e.Size = st.Size()
	e.ModTime = st.ModTime()
	if raw, err := os.ReadFile(path); err == nil {
		sum := md5.Sum(raw)
		e.MD5 = hex.EncodeToString(sum[:])
		e.Name = scriptDisplayName(path, raw)
	} else {
		e.Err = "读取失败: " + err.Error()
		return e
	}
	h, err := newLxHost(path)
	if err != nil {
		e.Err = err.Error()
		log.Printf("⚠️  音源加载失败（%s）: %v", file, err)
		return e
	}
	e.Host = h
	e.LoadedAt = time.Now()
	if !h.hasReq {
		e.Err = "脚本没有注册 request 处理器"
	}
	list := h.sourceList()
	for _, it := range list {
		e.Sources = append(e.Sources, it.Key)
	}
	if len(e.Sources) == 0 {
		e.Sources = []string{"（未声明，视为全支持）"}
	}
	return e
}

// 扫描音源：目录下所有 .js + 兼容旧的单文件 -script
func scanSources() {
	dir := cfg.SourcesDir
	seen := map[string]bool{}
	var next []*scriptEntry

	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("⚠️  音源目录创建失败: %v", err)
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			log.Printf("⚠️  音源目录读取失败: %v", err)
		}
		for _, de := range ents {
			if de.IsDir() || !strings.HasSuffix(strings.ToLower(de.Name()), ".js") {
				continue
			}
			p := filepath.Join(dir, de.Name())
			if abs, err := filepath.Abs(p); err == nil {
				seen[abs] = true
			}
			next = append(next, buildEntry(p, de.Name(), !scripts.isDisabled(de.Name())))
		}
	}

	// 兼容：-script 指定的单文件也当作一个音源（若它不在目录里）
	if cfg.ScriptPath != "" {
		if abs, err := filepath.Abs(cfg.ScriptPath); err == nil && !seen[abs] {
			if _, err := os.Stat(cfg.ScriptPath); err == nil {
				next = append(next, buildEntry(cfg.ScriptPath, filepath.Base(cfg.ScriptPath), !scripts.isDisabled(filepath.Base(cfg.ScriptPath))))
			}
		}
	}

	sort.SliceStable(next, func(i, j int) bool { return next[i].File < next[j].File })
	swapSources(next)
}

func swapSources(next []*scriptEntry) {
	srcMu.Lock()
	old := srcList
	srcList = next
	srcMu.Unlock()
	// 老宿主等飞行中的调用跑完再关
	for _, e := range old {
		if e.Host == nil {
			continue
		}
		keep := false
		for _, n := range next {
			if n.Host == e.Host {
				keep = true
				break
			}
		}
		if !keep {
			go func(h *lxHost) {
				time.Sleep(2 * time.Minute)
				close(h.closed)
			}(e.Host)
		}
	}
	logSources()
}

func logSources() {
	list := sourcesSnapshot()
	if len(list) == 0 {
		log.Printf("提示：没有可用音源脚本，只能解析网易免费曲（VIP 曲会失败）")
		return
	}
	for _, e := range list {
		state := "✅"
		extra := ""
		if e.Err != "" {
			state = "⚠️ "
			extra = "｜" + e.Err
		} else if !e.Enabled {
			state = "⏸ "
			extra = "｜已停用"
		}
		src := strings.Join(e.Sources, ",")
		log.Printf("%s 音源 %s（%s）｜声明: %s%s", state, e.Name, e.File, src, extra)
	}
}

// 重新加载某个文件（上传后 / 手动重载 / 远程更新后）
func reloadSourceFile(path, file string) error {
	e := buildEntry(path, file, !scripts.isDisabled(file))
	if e.Err != "" && e.Host == nil {
		return errors.New(e.Err)
	}
	srcMu.Lock()
	replaced := false
	for i, old := range srcList {
		if old.File == file {
			srcList[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		srcList = append(srcList, e)
		sort.SliceStable(srcList, func(i, j int) bool { return srcList[i].File < srcList[j].File })
	}
	srcMu.Unlock()
	logSources()
	return nil
}

func removeSourceFile(file string) (bool, error) {
	p := filepath.Join(cfg.SourcesDir, file)
	if err := os.Remove(p); err != nil {
		return false, err
	}
	srcMu.Lock()
	for i, e := range srcList {
		if e.File == file {
			srcList = append(srcList[:i], srcList[i+1:]...)
			break
		}
	}
	srcMu.Unlock()
	logSources()
	return true, nil
}

func setSourceEnabled(file string, on bool) {
	scripts.setDisabled(file, !on)
	srcMu.Lock()
	for _, e := range srcList {
		if e.File == file {
			e.Enabled = on
		}
	}
	srcMu.Unlock()
	logSources()
}

func sourceFiles() []string {
	list := sourcesSnapshot()
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.File)
	}
	sort.Strings(out)
	return out
}

func entryByFile(file string) *scriptEntry {
	srcMu.RLock()
	defer srcMu.RUnlock()
	for _, e := range srcList {
		if e.File == file {
			return e
		}
	}
	return nil
}

// ─────────────── 每个音源挂一份运行成绩（排序/熔断用，见 resolve.go） ───────────────
// 写一个音源文件（上传/远程更新都用它），成功后热替换该文件
func writeSourceFile(file string, data []byte, from string) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return errors.New("脚本内容为空")
	}
	if len(data) > 2<<20 {
		return fmt.Errorf("脚本过大（%d 字节，上限 2MB）", len(data))
	}
	file = sanitizeFileName(file)
	if file == "" {
		return errors.New("文件名不合法")
	}
	if cfg.SourcesDir == "" {
		return errors.New("未配置音源目录")
	}
	if err := os.MkdirAll(cfg.SourcesDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(cfg.SourcesDir, file)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if _, err := newLxHost(tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("脚本无法执行，已保留原脚本: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.WriteFile(path, data, 0o644)
	}
	if err := reloadSourceFile(path, file); err != nil {
		return err
	}
	log.Printf("📥 音源已更新（%s）: %s", from, file)
	return nil
}

// 文件名清洗：只留 basename，去掉路径分隔符，强制 .js 后缀
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.' || r == ' ':
			return r
		case r > 127: // 中文文件名放行
			return r
		}
		return -1
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == ".js" {
		return ""
	}
	if !strings.HasSuffix(strings.ToLower(name), ".js") {
		name += ".js"
	}
	return name
}
