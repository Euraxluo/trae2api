package server

import (
	_ "embed"
	"io"
	"net/http"
	"sort"
	"time"
)

//go:embed portal.html
var portalHTML string

// 本文件实现前台公开极客门户（`/`）—— 100% 对齐 workbody (WorkBuddy 2API) 前台门户。
func (s *Server) handlePortal(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 允许门户页面加载 Tailwind CDN 与 Google Fonts（与 workbody 完全一致）
	w.Header().Set("Content-Security-Policy",
		"default-src 'self' 'unsafe-inline' 'unsafe-eval' https: http: data: blob:; "+
			"script-src 'self' 'unsafe-inline' 'unsafe-eval' https://cdn.tailwindcss.com https://registry.npmmirror.com https://cdn.jsdelivr.net; "+
			"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
			"font-src 'self' https://fonts.gstatic.com data:; "+
			"img-src 'self' data: https: http:; "+
			"connect-src 'self' https: http:; frame-ancestors 'self'")
	_, _ = io.WriteString(w, portalHTML)
}

func (s *Server) handlePortalStatus(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := s.pool.Counts()
	realms := make([]string, 0, len(s.channels))
	chList := make([]map[string]any, 0, len(s.channels))
	modelsCount := 0

	for rk, ch := range s.channels {
		realms = append(realms, rk)
		cnt := 0
		if ch.Catalog != nil {
			cnt = len(ch.Catalog.Models)
		}
		if cnt > modelsCount {
			modelsCount = cnt
		}
		chList = append(chList, map[string]any{
			"realm":    rk,
			"label":    ch.Product.Label,
			"function": ch.Product.Function,
			"models":   cnt,
		})
	}
	sort.Strings(realms)
	sort.Slice(chList, func(i, j int) bool {
		return chList[i]["realm"].(string) < chList[j]["realm"].(string)
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"service":        ServiceName,
		"version":        Version,
		"total":          total,
		"healthy":        healthy,
		"realms":         realms,
		"models_count":   modelsCount,
		"channels":       chList,
		"status":         "online",
		"uptime_seconds": int(time.Since(s.startedAt).Seconds()),
	})
}
