package admin

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// NetworkHandler 管理「出站网络选项」—— 一份可复用的代理地址清单。
//
// 存在的理由：代理地址天然是**多个上游共用**的（同一个 clash / 公司代理），
// 而此前它以纯文本存在每个 provider 行上。改一次要改 N 处，且手填 URL
// 极易打错（少个斜杠、端口写错）—— 而打错不报错，只是连不上。
// 建成可枚举的清单后，新建上游从下拉里选，要新增点「新建网络」。
//
// 直连不是一条网络记录，而是 store.NetworkDirectID 这个保留 id（见该常量
// 的注释），由前端作为下拉的第一项展示。
type NetworkHandler struct {
	store *store.Store
}

func NewNetworkHandler(st *store.Store) *NetworkHandler {
	return &NetworkHandler{store: st}
}

type networkResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ProxyURL 是这条网络的代理地址（http/https/socks5/socks5h）。
	ProxyURL string `json:"proxy_url"`
	// ProviderCount 是引用它的上游数（派生值）。列表页用它提示
	// 「删掉这条会影响几个上游」—— 前端逐个查是 N+1。
	ProviderCount int   `json:"provider_count"`
	CreatedAt     int64 `json:"created_at"`
}

func toNetworkResponse(n store.Network) networkResponse {
	return networkResponse{
		ID: n.ID, Name: n.Name, ProxyURL: n.ProxyURL,
		ProviderCount: n.ProviderCount, CreatedAt: n.CreatedAt,
	}
}

// List 列出全部网络（不含直连那个保留 id —— 它不是一条记录）。
func (h *NetworkHandler) List(w http.ResponseWriter, r *http.Request) {
	networks, err := h.store.ListNetworks(r.Context())
	if err != nil {
		writeServerError(w, "list networks", err)
		return
	}
	result := make([]networkResponse, 0, len(networks))
	for _, n := range networks {
		result = append(result, toNetworkResponse(n))
	}
	writeJSON(w, http.StatusOK, result)
}

type networkRequest struct {
	Name     *string `json:"name"`
	ProxyURL *string `json:"proxy_url"`
}

func (h *NetworkHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req networkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(derefStr(req.Name))
	proxyURL := strings.TrimSpace(derefStr(req.ProxyURL))
	if name == "" {
		writeError(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if err := validateProxyURL(proxyURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n := &store.Network{ID: "net-" + generateID(), Name: name, ProxyURL: proxyURL}
	if err := h.store.CreateNetwork(r.Context(), n); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "已存在同名的网络")
			return
		}
		writeServerError(w, "create network", err)
		return
	}
	writeJSON(w, http.StatusCreated, toNetworkResponse(*n))
}

func (h *NetworkHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	var req networkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(derefStr(req.Name))
	proxyURL := strings.TrimSpace(derefStr(req.ProxyURL))
	if name == "" {
		writeError(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if err := validateProxyURL(proxyURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.UpdateNetwork(r.Context(), id, name, proxyURL); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "已存在同名的网络")
			return
		}
		writeNotFoundOrError(w, "update network", "网络不存在（可能已被并发删除）", err)
		return
	}
	writeJSON(w, http.StatusOK, networkResponse{ID: id, Name: name, ProxyURL: proxyURL})
}

// Delete 删除一条网络。引用它的上游由 FK 的 ON DELETE SET NULL 回到直连，
// 不删上游 —— 详见 store.DeleteNetwork 的注释。
func (h *NetworkHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteNetwork(r.Context(), id); err != nil {
		writeDeleteError(w, "delete network", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// validateProxyURL 在写入前校验代理地址。
//
// **为什么必须在这里校验**：此前 proxy_url 是逐 provider 手填的，填错要到
// 请求真的打过去才暴露（表现是 504 / 连接超时，且日志里只有一条 WARN）。
// 现在它是一份共享清单 —— 填错会让**所有**选它的上游一起连不上，
// 故障面从「一个上游」放大到「一批」。共享配置的校验必须在写入点，
// 而不是等到使用时。
func validateProxyURL(raw string) error {
	if raw == "" {
		return errors.New("proxy_url 不能为空（不代理请用「直连」，它不是一个网络条目）")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("proxy_url 不是合法的 URL（应形如 http://127.0.0.1:7897）")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return errors.New("proxy_url 的协议只支持 http / https / socks5 / socks5h，收到：" + u.Scheme)
	}
}
