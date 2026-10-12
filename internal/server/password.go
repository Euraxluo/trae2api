package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 本文件实现**控制台密码的运行时修改**（`POST /admin/password`）。
//
// ## 为什么密码落盘在 data/console.json 而不是回写 config.json
//
// 回写 config.json 意味着：读 JSON → 改一个字段 → 整个文件重新序列化。
// 那会**重排用户的字段顺序、丢掉注释、改写格式** —— 用户精心维护的配置文件
// 被工具改得面目全非，是很糟的体验（而且 JSON 本身不支持注释，用户可能
// 用了 JSONC）。
//
// 所以运行时改的密码存在独立的 `data/console.json`，启动时**优先于**
// `config.web_password`。两者都保留：config 是"部署时定的初值"，
// data 文件是"运行时改过的值"。
//
// ## 改密码为什么要核验原密码
//
// 控制台会话是 HttpOnly Cookie。若改密码不核验原密码，一个**已登录**的
// 会话（比如用户忘了锁屏）就能把密码改掉，把真正的主人锁在外面。
// 核验原密码把"能改密码"的权限收敛到"知道当前密码的人"。

// consoleSecretFile 是运行时密码的落盘结构。
type consoleSecretFile struct {
	// Password 为空表示**显式清除**（恢复单机免密）。
	//
	// 为什么要能区分"空"与"没有这个文件"：前者是用户主动清除密码，
	// 后者是"还没改过，用 config 里的值"。
	Password string `json:"password"`
	// UpdatedAt 仅用于诊断。
	UpdatedAt string `json:"updated_at,omitempty"`
}

// consoleSecretStore 管理运行时密码文件。
type consoleSecretStore struct {
	path string
	mu   sync.Mutex
}

func newConsoleSecretStore(dataDir string) *consoleSecretStore {
	return &consoleSecretStore{path: filepath.Join(dataDir, "console.json")}
}

// Load 读取运行时密码。返回 (密码, 是否存在该文件, 错误)。
//
// 文件不存在 → ("", false, nil)，调用方应回落到 config。
// 文件存在但为空密码 → ("", true, nil)，表示用户主动清除了密码。
func (s *consoleSecretStore) Load() (string, bool, error) {
	if s == nil || s.path == "" {
		return "", false, nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("读取 %s: %w", s.path, err)
	}
	var f consoleSecretFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", false, fmt.Errorf("解析 %s: %w", s.path, err)
	}
	return strings.TrimSpace(f.Password), true, nil
}

// Save 写入运行时密码（空串 = 清除）。
func (s *consoleSecretStore) Save(password string) error {
	if s == nil || s.path == "" {
		return fmt.Errorf("未配置数据目录，无法保存密码")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("创建目录: %w", err)
	}
	b, err := json.MarshalIndent(consoleSecretFile{
		Password:  strings.TrimSpace(password),
		UpdatedAt: nowRFC3339(),
	}, "", "  ")
	if err != nil {
		return err
	}
	// 原子写：避免半截 JSON 让下次启动读不出密码（那会把控制台**变成免密**，
	// 是安全上最糟的失败模式）。
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(s.path)
		if err2 := os.Rename(tmp, s.path); err2 != nil {
			os.Remove(tmp)
			return fmt.Errorf("替换 %s: %w", s.path, err2)
		}
	}
	return nil
}

// SetPassword 运行时更换密码（同时更新内存与落盘，并**作废所有会话**）。
//
// 作废所有会话是刻意的：改密码的常见动机之一就是"怀疑有人拿到了会话"，
// 此时让旧会话继续有效等于没改。
func (s *Server) SetPassword(newPassword string) error {
	if s.webAuth == nil {
		return fmt.Errorf("控制台密码闸未启用")
	}
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	if s.consoleSecret != nil {
		if err := s.consoleSecret.Save(newPassword); err != nil {
			return err
		}
	}
	s.webAuth.mu.Lock()
	s.webAuth.password = strings.TrimSpace(newPassword)
	s.webAuth.sessions = map[string]time.Time{} // 作废全部会话
	s.webAuth.mu.Unlock()
	return nil
}

// handlePasswordChange POST /admin/password
//
// body: {"old_password":"...","new_password":"..."}
//
// 规则（对齐参考实现）：
//   - 当前**有**密码时：必须核验 old_password
//   - 当前**无**密码时（单机免密）：允许直接设置（否则用户无法首次设密码）
//   - new_password 留空 → 清除密码，恢复单机免密
//   - new_password 非空 → 至少 6 位
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 核验原密码（当前有密码时）
	if s.webAuth != nil && s.webAuth.Enabled() {
		if !s.webAuth.Check(req.OldPassword) {
			s.logf("WARN: 改控制台密码时原密码核验失败")
			// 与登录失败同样计数，防止用它做密码爆破
			if s.loginLimit != nil {
				ip := clientIP(r, s.trustProxyHeaders())
				if locked, lockFor := s.loginLimit.NoteFailure(ip, time.Now()); locked {
					writeOpenAIError(w, http.StatusTooManyRequests, "too_many_attempts",
						fmt.Sprintf("尝试次数过多，请在 %d 分钟后再试", int(lockFor.Minutes())+1))
					return
				}
			}
			writeOpenAIError(w, http.StatusUnauthorized, "wrong_password", "原密码不正确")
			return
		}
	}

	if err := s.SetPassword(req.NewPassword); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_password", err.Error())
		return
	}

	cleared := strings.TrimSpace(req.NewPassword) == ""
	if cleared {
		s.logf("INFO: 控制台密码已清除（恢复单机免密）")
	} else {
		s.logf("INFO: 控制台密码已更新（所有旧会话已作废）")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"cleared": cleared,
		"hint":    "所有旧会话已作废，请用新密码重新登录 /console",
	})
}

// handlePasswordStatus GET /admin/password —— 报告密码闸状态（不泄露密码本身）。
func (s *Server) handlePasswordStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	enabled := s.webAuth != nil && s.webAuth.Enabled()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       enabled,
		"source":        s.passwordSource(),
		"min_length":    MinPasswordLen,
		"sessions":      s.webAuth.SessionCount(),
		"proxy_trusted": s.trustProxyHeaders(),
	})
}

// passwordSource 报告当前密码来自哪里（诊断用）。
func (s *Server) passwordSource() string {
	if s.consoleSecret != nil {
		if _, ok, _ := s.consoleSecret.Load(); ok {
			return "data/console.json（运行时设置）"
		}
	}
	if s.cfg != nil && strings.TrimSpace(s.cfg.WebPassword) != "" {
		return "config.web_password"
	}
	return "未设置（单机免密）"
}

// nowRFC3339 便于测试替换。
var nowRFC3339 = func() string { return time.Now().Format(time.RFC3339) }
