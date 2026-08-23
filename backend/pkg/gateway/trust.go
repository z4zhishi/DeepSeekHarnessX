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
//  2. 若带 Origin 头（浏览器场景），Origin 必须是 loopback 源；
//     无 Origin 的非浏览器客户端（Godot 原生、curl、SDK 本地进程）放行。
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

// originAllowed 校验 Origin 头：空 → 非浏览器本地客户端，放行；
// 存在 → 必须为 http(s)://<loopback> 源。
func originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return isLoopbackHost(u.Host)
}

// trustGuard 包装处理器：Host 与 Origin 双栅栏。
func (s *Server) trustGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden: untrusted Host", http.StatusForbidden)
			return
		}
		if !originAllowed(r.Header.Get("Origin")) {
			http.Error(w, "forbidden: untrusted Origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
