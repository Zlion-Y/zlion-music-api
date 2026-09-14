package main

// 直链校验：音源脚本返回的东西必须**真的能拉到音频字节**才算成功。
//
// 为什么必须有这一步：不少音源脚本返回的不是真直链，而是自己那套"解析接口"的地址
// （甚至不联网、1ms 就返回）。如果只用 "http 开头" 判定成功，这类最快返回的脚本案
// 件必然在竞速里胜出；一旦它指向的域名失效（实测 NXDOMAIN），每次取流拿到的都是死链，
// 而且死链还不计入失败率、坏源永远进不了熔断。
//
// 判定规则（HEAD 优先，接受 301/302 跟随；HEAD 不行再用 Range 拿两个字节）：
//   - 状态 200/206 且 Content-Type 是 audio/* / video/* / application/octet-stream（或缺失但有长度）
//     → 通过；
//   - 域名解析失败、超时、4xx/5xx → 不通过；
//   - Content-Type 是 JSON/HTML/文本 → 不通过（那是解析接口或错误页，不是音频）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// 返回 nil 表示这个直链可用；否则给出人能看懂的失败原因
func verifyAudio(ctx context.Context, rawURL string) error {
	if rawURL == "" {
		return errors.New("空直链")
	}
	timeout := cfg.VerifyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 1) 先 HEAD：最省流量
	if err := probeURL(cctx, http.MethodHead, rawURL); err == nil {
		return nil
	} else if isHardFail(err) {
		return err
	}
	// 2) HEAD 不被支持（405/403/501 之类）时再取两个字节
	if err := probeURL(cctx, http.MethodGet, rawURL); err != nil {
		return err
	}
	return nil
}

// 部分硬失败（DNS/连接层/内容不对）没必要再试第二次
func isHardFail(err error) bool {
	var ve *verifyError
	if errors.As(err, &ve) {
		return ve.hard
	}
	return false
}

type verifyError struct {
	msg  string
	hard bool
}

func (e *verifyError) Error() string { return e.msg }

func vErr(hard bool, format string, a ...any) error {
	return &verifyError{msg: fmt.Sprintf(format, a...), hard: hard}
}

func probeURL(ctx context.Context, method, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return vErr(true, "直链格式不合法")
	}
	req.Header.Set("User-Agent", uaDesktop)
	req.Header.Set("Accept", "*/*")
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-1") // 只要头两个字节
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return vErr(true, "域名解析失败（%s 不存在）", dnsErr.Name)
		}
		if ctx.Err() != nil {
			return vErr(true, "校验超时")
		}
		msg := err.Error()
		if strings.Contains(msg, "context deadline") {
			return vErr(true, "校验超时")
		}
		return vErr(true, "连接失败（%s）", shorten(msg, 60))
	}
	defer resp.Body.Close()
	// 只读一点点，然后直接关掉（不下载整首歌）
	_, _ = io.CopyN(io.Discard, resp.Body, 2048)

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden && method == http.MethodHead:
		return vErr(false, "HEAD 不被支持") // 软失败：换个方法再试
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return vErr(true, "被拒绝（HTTP %d）", resp.StatusCode)
	case resp.StatusCode >= 400:
		return vErr(true, "HTTP %d", resp.StatusCode)
	}

	ctype := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	switch {
	case ctype == "":
		// 没给类型：至少要有点内容
		if resp.ContentLength == 0 && resp.StatusCode != http.StatusPartialContent {
			return vErr(true, "返回内容为空")
		}
		return nil
	case strings.HasPrefix(ctype, "audio/"), strings.HasPrefix(ctype, "video/"),
		ctype == "application/octet-stream", ctype == "binary/octet-stream",
		ctype == "application/ogg", ctype == "application/x-mpegurl", ctype == "application/vnd.apple.mpegurl":
		return nil
	case strings.Contains(ctype, "json"), strings.HasPrefix(ctype, "text/"), strings.Contains(ctype, "html"):
		return vErr(true, "返回的不是音频（%s），多半是解析接口或错误页", ctype)
	default:
		return vErr(true, "返回的不是音频（%s）", ctype)
	}
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
