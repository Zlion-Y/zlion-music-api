package main

// 取流调度：多音源不是"全打一遍"，而是按成绩排序、分波下发、赢家一出立刻掐掉其余。
//
// 为什么这么做：
//   - 全打一遍意味着每次解析都向上游发 N 个请求，音源一多就容易被限流/封 IP；
//   - 赢家出现后输的那些还会继续跑到超时（最长 20s），白白占着连接和 goroutine。
//
// 规则：
//   1. 每个音源记录成功率、平均耗时、连续失败次数，按成绩排序（熔断的排最后）；
//   2. 一波最多同时问 `-resolve-concurrency` 个（默认 3）；
//   3. 一波内全失败、或等了 `-resolve-hedge` 还没结果，就再放一波（补位）；
//   4. 一旦有人成功：立刻返回，并取消这一轮里其它音源的在途 HTTP 请求；
//   5. 连续失败到阈值就熔断 `-quarantine` 时长（默认 10 分钟），期间不再问它，管理页可手动恢复。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// 体检结果里的单条
type probeResult struct {
	File        string         `json:"file"`
	Name        string         `json:"name"`
	Sources     string         `json:"sources"`
	Enabled     bool           `json:"enabled"`
	OK          bool           `json:"ok"`
	URL         string         `json:"url,omitempty"`
	Via         string         `json:"via,omitempty"`
	Elapsed     int64          `json:"elapsedMs"`
	Error       string         `json:"error,omitempty"`
	Quarantined bool           `json:"quarantined,omitempty"`
	Stats       map[string]any `json:"stats,omitempty"`
}

// 每个音源的运行成绩
type srcStats struct {
	mu          sync.Mutex
	attempts    int64
	successes   int64
	consecFails int64
	latSamples  int64
	latency     float64 // 指数滑动平均（毫秒）
	lastErr     string
	quarantine  time.Time
	lastOK      time.Time
}

func (s *srcStats) record(ok bool, ms float64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if ok {
		s.successes++
		s.consecFails = 0
		s.lastErr = ""
		s.quarantine = time.Time{}
		s.lastOK = time.Now()
		if s.latSamples == 0 {
			s.latency = ms
		} else {
			s.latency = s.latency*0.7 + ms*0.3
		}
		s.latSamples++
		return
	}
	s.consecFails++
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = "没有返回可用直链"
	}
	if cfg.Quarantine > 0 && s.consecFails >= 3 {
		s.quarantine = time.Now().Add(cfg.Quarantine)
	}
}

func (s *srcStats) quarantined() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.quarantine)
}

func (s *srcStats) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consecFails = 0
	s.quarantine = time.Time{}
}

// 排序分数：越大越先问。新音源给中性分（(1)/(0+2)=0.5），
// 有成绩的按 成功率 排序，同样成功率下快的优先；熔断中的直接沉底。
func (s *srcStats) score() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.quarantine) {
		return -1e6
	}
	rate := float64(s.successes+1) / float64(s.attempts+2)
	penalty := 0.0
	if s.latency > 0 {
		penalty = s.latency / 20000 // 20 秒封顶，约等于 0~0.05
	}
	return rate - penalty
}

func (s *srcStats) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"attempts":    s.attempts,
		"successes":   s.successes,
		"consecFails": s.consecFails,
		"lastError":   s.lastErr,
	}
	if s.attempts > 0 {
		out["successRate"] = float64(int(float64(s.successes)/float64(s.attempts)*1000+0.5)) / 10
	}
	if s.latSamples > 0 {
		out["latencyMs"] = int(s.latency + 0.5)
	}
	if time.Now().Before(s.quarantine) {
		out["quarantined"] = true
		out["quarantineLeft"] = int(time.Until(s.quarantine).Seconds() + 0.5)
	}
	if !s.lastOK.IsZero() {
		out["lastOk"] = s.lastOK.Format("15:04:05")
	}
	return out
}

// 按成绩排序候选音源
func rankedHosts(source string) []*scriptEntry {
	list := pickHosts(source)
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].stats.score() > list[j].stats.score()
	})
	return list
}

type attempt struct {
	entry *scriptEntry
	v     any
	err   error
	ms    float64
}

// 分波取流：谁先成功用谁，其余立刻取消。
// 返回 (结果, 音源名, 错误)
func resolveWithHedge(ctx context.Context, source, action string, info map[string]any) (any, string, error) {
	cands := rankedHosts(source)
	if len(cands) == 0 {
		return nil, "", errors.New("没有可用的音源脚本")
	}
	// 全都在熔断里：临时忽略熔断再试一轮，避免"全都熔断=彻底不可用"
	allQuarantined := true
	for _, e := range cands {
		if !e.stats.quarantined() {
			allQuarantined = false
			break
		}
	}
	if allQuarantined {
		debugf("所有音源都在熔断期，临时忽略熔断重试一轮")
	}

	roundCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	wave := cfg.ResolveConcurrency
	if wave <= 0 {
		wave = 3
	}
	hedge := cfg.ResolveHedge
	if hedge <= 0 {
		hedge = 300 * time.Millisecond
	}

	ch := make(chan attempt, len(cands))
	launched, inFlight := 0, 0
	var firstErr error
	var errSource string

	launch := func(e *scriptEntry) {
		inFlight++
		go func() {
			t0 := time.Now()
			v, err := e.Host.invoke(roundCtx, source, action, info)
			ms := float64(time.Since(t0).Milliseconds())
			// 本轮已经被别人赢了、这个请求是被取消的：不算它的失败，
			// 否则健康音源会因为"竞速输了"被记成失败甚至熔断
			if roundCtx.Err() == nil {
				e.stats.record(err == nil && v != nil, ms, err)
			}
			ch <- attempt{entry: e, v: v, err: err, ms: ms}
		}()
	}

	deadline := time.Now().Add(hedge)
	for {
		for inFlight < wave && launched < len(cands) {
			launch(cands[launched])
			launched++
		}
		if inFlight == 0 {
			break // 没在跑的也没可发的了
		}
		wait := time.Until(deadline)
		if wait < 0 {
			wait = 0
		}
		select {
		case a := <-ch:
			inFlight--
			if a.err == nil && a.v != nil {
				cancel() // 赢家已定：掐掉其余在途请求
				return a.v, a.entry.Name, nil
			}
			if firstErr == nil {
				firstErr = a.err
				errSource = a.entry.Name
				if firstErr == nil {
					firstErr = errors.New("没有返回可用直链")
				}
			}
			// 失败空出位置：循环顶部会立刻补一个
		case <-time.After(wait):
			if launched >= len(cands) {
				// 没有更多可发的了，就等这一波出结果
				a := <-ch
				inFlight--
				if a.err == nil && a.v != nil {
					cancel()
					return a.v, a.entry.Name, nil
				}
				if firstErr == nil {
					firstErr = a.err
					errSource = a.entry.Name
				}
			}
			deadline = time.Now().Add(hedge) // 再放一波
		case <-ctx.Done():
			cancel()
			return nil, "", ctx.Err()
		}
	}
	if firstErr == nil {
		firstErr = errors.New("所有音源都没有返回结果")
	}
	if errSource != "" {
		firstErr = fmt.Errorf("%s: %w", errSource, firstErr)
	}
	return nil, "", firstErr
}

// 体检：同样的并发控制，但要等所有音源的结果（不提前返回），用于看全貌
func probeAllSources(ctx context.Context, source, action string, info map[string]any) []probeResult {
	entries := sourcesSnapshot()
	results := make([]probeResult, len(entries))
	wave := cfg.ResolveConcurrency
	if wave <= 0 {
		wave = 3
	}
	sem := make(chan struct{}, wave)
	var wg sync.WaitGroup
	for i, e := range entries {
		results[i] = probeResult{
			File: e.File, Name: e.Name, Enabled: e.Enabled,
			Sources: joinSources(e),
		}
		if e.Host == nil {
			results[i].Error = firstNonEmpty(e.Err, "未加载")
			continue
		}
		if !e.Enabled {
			results[i].Error = "已停用"
			continue
		}
		if !e.Host.supports(source) {
			results[i].Error = "该音源不支持 " + source
			continue
		}
		if q := e.stats.quarantined(); q {
			results[i].Quarantined = true
		}
		wg.Add(1)
		go func(i int, e *scriptEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			v, err := e.Host.invoke(ctx, source, action, info)
			ms := time.Since(t0).Milliseconds()
			results[i].Elapsed = ms
			ok := false
			if err != nil {
				results[i].Error = err.Error()
			} else if link, isURL := v.(string); isURL && len(link) > 7 && (link[:4] == "http") {
				ok = true
				results[i].OK = true
				results[i].URL = link
				results[i].Via = "script"
			} else {
				results[i].Error = "没有返回可用直链"
			}
			e.stats.record(ok, float64(ms), err)
			results[i].Stats = e.stats.snapshot()
		}(i, e)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].OK != results[j].OK {
			return results[i].OK
		}
		return results[i].Elapsed < results[j].Elapsed
	})
	return results
}

func joinSources(e *scriptEntry) string {
	out := ""
	for i, s := range e.Sources {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
