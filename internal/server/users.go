package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"trae2api/internal/usermgr"
)

// 本文件提供用户与角色管理端点（全部要求管理员权限）。
//
// 端点一览：
//
//	GET    /admin/users                    列出用户（API Key 打码）
//	POST   /admin/users                    创建用户 {username, role, note}
//	DELETE /admin/users/{id}               删除用户
//	POST   /admin/users/{id}/reset-key     重置 API Key（返回新 Key，仅此一次可见）
//	POST   /admin/users/{id}/disable|enable 启停用户

// handleUsers GET /admin/users 或 POST /admin/users
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listUsers(w)
	case http.MethodPost:
		s.createUser(w, r)
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 GET / POST")
	}
}

func (s *Server) listUsers(w http.ResponseWriter) {
	users := s.users.List()
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id": u.ID, "username": u.Username,
			// Key 打码：列表接口不该泄露可用凭证
			"api_key_masked": usermgr.MaskKey(u.APIKey),
			"role":           u.Role,
			"created_at":     u.CreatedAt,
			"disabled":       u.Disabled,
			"note":           u.Note,
			"quota":          u.Quota,
			"used_quota":     u.UsedQuota,
			"model_whitelist": u.ModelWhitelist,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out, "count": len(out)})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Note     string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	role := usermgr.Role(strings.TrimSpace(req.Role))
	if role == "" {
		role = usermgr.RoleUser
	}
	u, err := s.users.Create(req.Username, role, req.Note)
	if err != nil {
		// 用户名重复等属客户端错误；落盘失败属服务端问题但用户已在内存生效
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "落盘失败") {
			status = http.StatusInternalServerError
		}
		writeOpenAIError(w, status, "create_user_failed", err.Error())
		if u == nil {
			return
		}
	}
	// API Key 仅在此处明文返回一次（之后列表只给打码值）
	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role,
		"api_key": u.APIKey,
		"warning": "API Key 仅本次返回明文，请立即保存；后续只能在列表中看到打码值。",
	})
}

// handleUserAction 处理 /admin/users/{id}/{action}。
func (s *Server) handleUserAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少用户 id")
			return
		}
		switch action {
		case "role":
			// 改角色。⚠️ 不能把最后一个管理员降级（usermgr.SetRole 会拒绝）——
			// 否则管理面永久锁死，与"首个用户必须是管理员"防的是同一件事。
			var req struct {
				Role string `json:"role"`
			}
			if err := decodeJSON(r, &req); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			u, ok := s.users.ByID(id)
			if !ok {
				writeOpenAIError(w, http.StatusNotFound, "not_found", "用户不存在")
				return
			}
			old := u.Role
			if err := s.users.SetRole(id, usermgr.Role(req.Role)); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "role_change_failed", err.Error())
				return
			}
			s.logf("INFO: 用户 %s 的角色 %s → %s", u.Username, old, req.Role)
			writeJSON(w, http.StatusOK, map[string]any{
				"id": id, "username": u.Username, "role": req.Role, "previous_role": old,
			})
		case "reset-key":
			targetID := id
			if targetID == "admin" {
				var adminUser *usermgr.User
				for _, item := range s.users.List() {
					if item.Role == usermgr.RoleAdmin {
						adminUser, _ = s.users.ByID(item.ID)
						break
					}
				}
				if adminUser == nil {
					var err error
					adminUser, err = s.users.Create("admin", usermgr.RoleAdmin, "控制台默认管理员")
					if err != nil && adminUser == nil {
						writeOpenAIError(w, http.StatusInternalServerError, "admin_create_failed", "初始化管理员账号失败: "+err.Error())
						return
					}
				}
				targetID = adminUser.ID
			}
			u, err := s.users.ResetKey(targetID)
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "reset_key_failed", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id": u.ID, "username": u.Username, "api_key": u.APIKey,
				"warning": "旧 API Key 已立即失效；新 Key 仅本次返回明文。",
			})
		case "quota":
			var req struct {
				Quota int64 `json:"quota"`
			}
			if err := decodeJSON(r, &req); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			if err := s.users.SetQuota(id, req.Quota); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "quota_update_failed", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "quota": req.Quota})
		case "whitelist":
			var req struct {
				Models []string `json:"models"`
			}
			if err := decodeJSON(r, &req); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			if err := s.users.SetModelWhitelist(id, req.Models); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "whitelist_update_failed", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "model_whitelist": req.Models})
		case "disable", "enable":
			disabled := action == "disable"
			if err := s.users.SetDisabled(id, disabled); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, action+"_failed", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "disabled": disabled})
		default:
			writeOpenAIError(w, http.StatusNotFound, "not_found", "未知操作: "+action)
		}
	}
}

// handleUserDelete DELETE /admin/users/{id}
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := s.users.Delete(id); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "delete_user_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// decodeJSON 读取并解析请求体（限制 1MB，防超大 body 打爆内存）。
func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取请求体: %w", err)
	}
	if len(buf) == 0 {
		return nil
	}
	if err := json.Unmarshal(buf, out); err != nil {
		return fmt.Errorf("解析请求体: %w", err)
	}
	return nil
}
