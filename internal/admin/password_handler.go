package admin

import (
	"errors"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/adminauth"
)

// PasswordHandler 暴露管理密码的设置与状态查询。
//
// 凭据的存取全部委托给 adminauth.Store —— 那个包同时被 HTTP 中间件使用，
// 因此「改密码」与「校验请求」看到的是同一份运行时状态，
// 改完立即生效，不需要重启网关。
type PasswordHandler struct {
	auth *adminauth.Store
}

func NewPasswordHandler(auth *adminauth.Store) *PasswordHandler {
	return &PasswordHandler{auth: auth}
}

type setPasswordRequest struct {
	Password string `json:"password"`
}

type passwordStatusResponse struct {
	HasPassword bool `json:"has_password"`
	// FirstSetup 为真表示系统里还没有任何凭据，前端应显示「设置管理密码」，
	// 而不是「请输入管理员密码」—— 后者会让用户去猜一个根本不存在的密码。
	FirstSetup bool `json:"first_setup"`
	// Source 说明当前凭据来自哪里，纯粹用于排障展示：
	// "password_file"（后台设置过）/"config_token"（仍在使用配置文件里的初始令牌）
	// /"locked"（凭据文件损坏，见 Locked）。
	Source string `json:"source"`
	// Locked 为真表示凭据文件存在但不可用，后台进入锁定态。
	//
	// 这时 HasPassword 也为真（否则 password/set 的引导窗口会对所有人敞开，
	// 一个损坏的文件就变成「后台任人接管」），前端必须优先看 Locked，
	// 提示用户删文件重启，而不是反复让他猜密码。
	Locked  bool   `json:"locked,omitempty"`
	Message string `json:"message,omitempty"`
}

// Check 报告凭据状态。该端点经中间件豁免，无需鉴权：
// 响应里只有一个布尔值和来源标签，不含任何可用于登录的信息 ——
// 也不能含凭据文件的绝对路径（文件系统布局本身就是不该外泄的信息）。
func (h *PasswordHandler) Check(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.LockedError(); err != nil {
		writeJSON(w, http.StatusOK, passwordStatusResponse{
			HasPassword: true,
			Source:      "locked",
			Locked:      true,
			Message: "管理凭据文件已损坏或不可读，管理后台暂时锁定（转发服务不受影响）。" +
				"删除状态目录下的 admin_auth.json 后重启网关即可重新设置密码。",
		})
		return
	}

	resp := passwordStatusResponse{HasPassword: h.auth.HasCredential()}
	if !resp.HasPassword {
		resp.Source = "none"
		resp.FirstSetup = true
	} else if h.auth.HasUserPassword() {
		resp.Source = "password_file"
	} else {
		resp.Source = "config_token"
	}
	writeJSON(w, http.StatusOK, resp)
}

// Set 设置或更新管理密码。
//
// 鉴权由中间件完成：系统已有凭据时，该请求必须带上正确的旧凭据才会被放行。
func (h *PasswordHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req setPasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if err := h.auth.Set(req.Password); err != nil {
		if errors.Is(err, adminauth.ErrWeakPassword) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 写盘失败的原文含凭据文件的绝对路径，进日志不回显。
		writeServerError(w, "set admin password", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "密码已更新并立即生效",
		// 落盘位置随响应返回，方便用户在被锁在外面时知道该删哪个文件。
		"credential_file": h.auth.Path(),
	})
}

// Verify 校验当前请求携带的凭据是否有效。
//
// 存在的意义是让前端能「先验证再保存」：走到这个 handler 说明中间件已经
// 放行，即令牌有效。旧前端把用户输入直接写进 localStorage 再刷新，
// 输入错误时会被 401 弹回同一个对话框，表现为「一直重复要求输入」。
func (h *PasswordHandler) Verify(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
