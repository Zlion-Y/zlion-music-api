package main

// 宿主全局对象：洛雪客户端运行时里有、而 goja 默认没有的东西。
//
// 实测 21 个流传的音源脚本，有 9 个因为缺 `console`、3 个因为缺 `setTimeout`
// **直接加载失败**（ReferenceError），跟音源本身是否可用无关。这里补齐：
// console / setTimeout / setInterval / clearTimeout / clearInterval，
// 其余（Buffer、fetch、btoa、TextEncoder…）由引导脚本用 JS 垫片实现。

import (
	"encoding/base64"
	"encoding/hex"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

func (h *lxHost) injectGlobals(vm *goja.Runtime) {
	// 引导脚本里的 console 最终落到这里：warn/error 一直打，log/info/debug 只在 -debug 时打
	vm.Set("__lxLog", func(call goja.FunctionCall) goja.Value {
		always := len(call.Arguments) > 2 && call.Argument(2).ToBoolean()
		if !always && !cfg.Debug {
			return goja.Undefined()
		}
		log.Printf("[音源 %s] %s: %s", h.scriptName, call.Argument(0).String(), call.Argument(1).String())
		return goja.Undefined()
	})
	// 引导脚本里的 Buffer / TextEncoder 等垫片用的字节工具
	vm.Set("bufFromGo", func(call goja.FunctionCall) goja.Value {
		s := call.Argument(0).String()
		switch strings.ToLower(call.Argument(1).String()) {
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
	})
	vm.Set("bufToStringGo", func(call goja.FunctionCall) goja.Value {
		b := toBytes(call.Argument(0))
		switch strings.ToLower(call.Argument(1).String()) {
		case "base64":
			return vm.ToValue(base64.StdEncoding.EncodeToString(b))
		case "hex":
			return vm.ToValue(hex.EncodeToString(b))
		}
		return vm.ToValue(string(b))
	})
	h.injectTimers(vm)
}

func (h *lxHost) injectTimers(vm *goja.Runtime) {
	h.timerMu.Lock()
	if h.timers == nil {
		h.timers = map[int64]bool{}
	}
	h.timerMu.Unlock()

	// 标记取消：true 表示已取消
	isCancelled := func(id int64) bool {
		h.timerMu.Lock()
		defer h.timerMu.Unlock()
		return h.timers[id]
	}

	schedule := func(call goja.FunctionCall, repeat bool) goja.Value {
		fn, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			return goja.Null()
		}
		ms := call.Argument(1).ToInteger()
		if ms < 0 {
			ms = 0
		}
		var extra []goja.Value
		if len(call.Arguments) > 2 {
			extra = append(extra, call.Arguments[2:]...)
		}
		id := atomic.AddInt64(&h.timerSeq, 1)
		h.timerMu.Lock()
		h.timers[id] = false
		h.timerMu.Unlock()

		go func() {
			for {
				if ms > 0 {
					time.Sleep(time.Duration(ms) * time.Millisecond)
				}
				select {
				case <-h.closed:
					return
				default:
				}
				if isCancelled(id) {
					return
				}
				// 回调必须回到 VM 线程
				h.post(func() {
					if isCancelled(id) {
						return
					}
					defer func() {
						if r := recover(); r != nil {
							log.Printf("音源 %s 的定时器回调出错: %v", h.scriptName, r)
						}
					}()
					_, _ = fn(goja.Undefined(), extra...)
				})
				if !repeat {
					h.timerMu.Lock()
					delete(h.timers, id)
					h.timerMu.Unlock()
					return
				}
				if ms == 0 {
					ms = 1 // 防 setInterval(fn, 0) 把 CPU 打满
				}
			}
		}()
		return vm.ToValue(id)
	}

	vm.Set("setTimeout", func(call goja.FunctionCall) goja.Value { return schedule(call, false) })
	vm.Set("setInterval", func(call goja.FunctionCall) goja.Value { return schedule(call, true) })
	clearFn := func(call goja.FunctionCall) goja.Value {
		id := call.Argument(0).ToInteger()
		h.timerMu.Lock()
		h.timers[id] = true
		h.timerMu.Unlock()
		return goja.Undefined()
	}
	vm.Set("clearTimeout", clearFn)
	vm.Set("clearInterval", clearFn)
}
