package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// WebhookNotifier 负责向外部通道推送告警通知。
type WebhookNotifier struct {
	url     string
	secret  string
	enabled bool

	mu       sync.Mutex
	lastSent map[string]time.Time // 防轰炸：同类告警 5 分钟内只发一次
}

func newWebhookNotifier(url, secret string, enabled bool) *WebhookNotifier {
	return &WebhookNotifier{
		url:      url,
		secret:   secret,
		enabled:  enabled && url != "",
		lastSent: make(map[string]time.Time),
	}
}

// SendAlert 发送一条告警。
// title: 告警标题
// message: 详细说明
// level: "warning" | "error" | "info"
func (n *WebhookNotifier) SendAlert(title, message, level string) {
	if n == nil || !n.enabled || n.url == "" {
		return
	}

	key := title + ":" + message
	n.mu.Lock()
	last, ok := n.lastSent[key]
	if ok && time.Since(last) < 5*time.Minute {
		n.mu.Unlock()
		return
	}
	n.lastSent[key] = time.Now()
	n.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()

		// 构造兼容常见 Webhook（如企业微信、飞书、钉钉自定义机器人及标准 HTTP JSON）的通用负载
		payload := map[string]any{
			"msg_type": "text",
			"content": map[string]any{
				"text": fmt.Sprintf("[%s] %s\n%s\n时间: %s", stringsToUpper(level), title, message, time.Now().Format("2006-01-02 15:04:05")),
			},
			// 企业微信格式
			"text": map[string]any{
				"content": fmt.Sprintf("[%s] %s\n%s", stringsToUpper(level), title, message),
			},
			// 标准 JSON 字段
			"title":     title,
			"message":   message,
			"level":     level,
			"timestamp": time.Now().Unix(),
		}

		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(raw))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if n.secret != "" {
			req.Header.Set("X-Webhook-Secret", n.secret)
		}

		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Do(req)
		if err == nil && resp != nil {
			_ = resp.Body.Close()
		}
	}()
}

func stringsToUpper(s string) string {
	switch s {
	case "error":
		return "🚨 ERROR"
	case "warning":
		return "⚠️ WARNING"
	default:
		return "ℹ️ INFO"
	}
}
