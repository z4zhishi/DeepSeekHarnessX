package gateway

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// CSRF/Host 信任栅栏（设计稿 Phase 6「网关：CSRF/Host 信任栅栏」）：
// 网关只监听 loopback（127.0.0.1），任何到达 /api/* 的请求必须满足：
//  1. Host 头是 loopback 名（127.0.0.1 / localhost / ::1）；
//  2. 若带 Sec-Fetch-Site: cross-site（浏览器跨站发起），无条件拒绝；
//  3. 若带 Origin 头（浏览器场景），Origin 必须与请求 Host 权威精确一致
//     （host 大小写不敏感 + 端口规范化比较；同机不同端口的源一律拒绝）；
//     无 Origin 且无 Sec-Fetch-Site 的非浏览器客户端（Godot 原生、curl、
//     SDK 本地进程）照常放行。
// 浏览器跨站 POST（CSRF/DNS rebinding）因此被 403 拒绝。

// isLoopbackHost 判断 Host 头是否为回环地址（含端口剥离）。
func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		return false
	}
	// 剥离端口：仅当恰好一个冒号时视为 host:port；方括号 IPv6 或裸 IPv6（多冒号）不剥离。
	if strings.HasPrefix(h, "[") {
		if idx := strings.Index(h, "]"); idx >= 0 {
			h = h[1:idx]
		}
	} else if strings.Count(h, ":") == 1 {
		if idx := strings.LastIndex(h, ":"); idx >= 0 {
			h = h[:idx]
		}
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// splitAuthority 拆分 host[:port] 权威（Host 头或 Origin URL.Host），展开
// IPv6 方括号。裸 IPv6（多冒号无方括号）整体视为主机、无端口。
func splitAuthority(authority string) (host, port string, ok bool) {
	h := strings.TrimSpace(authority)
	if h == "" {
		return "", "", false
	}
	if strings.HasPrefix(h, "[") {
		end := strings.Index(h, "]")
		if end < 0 {
			return "", "", false
		}
		host = h[1:end]
		if len(h) > end+1 && h[end+1] == ':' {
			port = h[end+2:]
		}
		return host, port, true
	}
	if idx := strings.LastIndex(h, ":"); idx >= 0 && strings.Count(h, ":") == 1 {
		return h[:idx], h[idx+1:], true
	}
	return h, "", true
}

// defaultPortFor 返回 scheme 的缺省端口（网关仅为明文 HTTP/HTTPS 场景设计）。
func defaultPortFor(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// originMatchesHost 校验 Origin 与请求 Host 权威精确一致：主机大小写不敏感，
// 缺省端口按 scheme 归一（Host 头不带 scheme，按 http 的 80 归一）。
func originMatchesHost(origin, reqHost string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	ohost, oport, ok := splitAuthority(u.Host)
	if !ok || ohost == "" {
		return false
	}
	hhost, hport, ok := splitAuthority(reqHost)
	if !ok || hhost == "" {
		return false
	}
	if oport == "" {
		oport = defaultPortFor(u.Scheme)
	}
	if hport == "" {
		hport = defaultPortFor("http")
	}
	return strings.EqualFold(ohost, hhost) && oport == hport
}

// originAllowed 校验 Origin 头（收紧后语义）：空 → 非浏览器本地客户端，放行；
// 存在 → 必须与请求 Host 权威精确一致（此前仅要求任意回环源，同机不同端口
// 也放行，属有意收紧的行为变更）。
func originAllowed(origin, reqHost string) bool {
	if origin == "" {
		return true
	}
	return originMatchesHost(origin, reqHost)
}

// requestTrusted 是 HTTP 与 WS upgrade 共用的栅栏谓词：
// loopback Host 必需；Sec-Fetch-Site: cross-site 无条件拒绝；浏览器 Origin
// 与请求权威精确一致；无 Origin 无 Sec-Fetch-Site 的本地非浏览器客户端放行。
func requestTrusted(host, origin, secFetchSite string) bool {
	if !isLoopbackHost(host) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(secFetchSite), "cross-site") {
		return false
	}
	return originAllowed(origin, host)
}

// trustGuard 包装处理器：Host / Sec-Fetch-Site / Origin 三重栅栏。
func (s *Server) trustGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requestTrusted(r.Host, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site")) {
			http.Error(w, "forbidden: untrusted Host/Origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
