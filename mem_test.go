package main

// 量一下「多音源到底吃多少资源」：每个音源是一个独立的 goja 运行时，
// 这个测试把一批 .js 全部加载起来，报告加载耗时、堆增量、goroutine 数。
//
// 跑法：LX_DIR="C:/Users/xxx/Desktop/V260912" go test -run TestSourceMemory -v

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSourceMemory(t *testing.T) {
	dir := os.Getenv("LX_DIR")
	ents, err := filepath.Glob(filepath.Join(dir, "*.js"))
	if err != nil || len(ents) == 0 {
		t.Skipf("用 LX_DIR=<你的音源目录> 指定一批 .js 再跑，例如：LX_DIR=~/lx-sources go test -run TestSourceMemory -v")
	}

	// 测试环境没有 flag.Parse，手动给几个默认值
	cfg = Config{Quality: "320k", Timeout: 12 * time.Second, InvokeTimeout: 20 * time.Second, TTL: 15 * time.Minute}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	g0 := runtime.NumGoroutine()

	start := time.Now()
	var hosts []*lxHost
	loaded, failed := 0, 0
	for _, p := range ents {
		h, err := newLxHost(p)
		if err != nil {
			failed++
			continue
		}
		hosts = append(hosts, h)
		loaded++
	}
	loadDur := time.Since(start)

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	perHost := float64(after.HeapAlloc-before.HeapAlloc) / float64(len(hosts))
	t.Logf("脚本目录: %s", dir)
	t.Logf("加载 %d 个成功 / %d 个失败，耗时 %v（平均 %.0fms/个）", loaded, failed, loadDur.Round(time.Millisecond), float64(loadDur.Milliseconds())/float64(len(hosts)))
	t.Logf("堆增量: %.1f MB（每个音源约 %.0f KB）", float64(after.HeapAlloc-before.HeapAlloc)/1024/1024, perHost/1024)
	t.Logf("HeapObjects 增量: %d", after.HeapObjects-before.HeapObjects)
	t.Logf("goroutine: %d -> %d（每个音源 1 个 VM 线程 + 可能的定时器）", g0, runtime.NumGoroutine())

	// 空闲 3 秒看有没有脚本在后台空转
	runtime.GC()
	var idle1 runtime.MemStats
	runtime.ReadMemStats(&idle1)
	time.Sleep(3 * time.Second)
	runtime.GC()
	var idle2 runtime.MemStats
	runtime.ReadMemStats(&idle2)
	t.Logf("空闲 3 秒后堆变化: %.2f MB（对比总堆 %.1f MB）",
		float64(int64(idle2.HeapAlloc)-int64(idle1.HeapAlloc))/1024/1024, float64(idle2.HeapAlloc)/1024/1024)
	t.Logf("goroutine 数（空闲）: %d", runtime.NumGoroutine())

	for _, h := range hosts {
		close(h.closed)
	}
}
