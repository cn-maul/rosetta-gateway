package admin

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 站点品牌（站点名称 + LOGO）的两条管理端点。
//
// 单独一个 handler 而不是塞进 SettingsHandler：这两条端点的鉴权口径与
// SettingsHandler 恰好相反 —— GET 必须**免鉴权**（登录页还没有会话时要先
// 拿到品牌才能渲染），其余设置端点全部需要管理员。把两种口径放在同一个
// handler 里，后来的人很难一眼看出哪条是公开的，而这正是最容易出错的地方。

const (
	// maxSiteNameRunes 与前端 SiteBrandCard 的 MAX_NAME_RUNES 一致。
	// 上限按 rune 而非 byte 计：中文名 3 字节/字，按 byte 算会让中文名
	// 只允许 21 个字，用户看到的是「英文能填 64 个、中文只能 21 个」。
	maxSiteNameRunes = 64

	// maxSiteLogoBytes 是 LOGO **解码后**的字节上限（256KB）。
	//
	// 为什么卡解码后而不是 base64 字符串长度：LOGO 最终是塞进每个页面
	// 的 <img src>，也就是每次请求都要随 HTML 再传一遍；限制 base64 长度
	// 只限制了传输体积的一半，解码后的原图才是浏览器实际要渲染的东西。
	maxSiteLogoBytes = 256 << 10

	// siteLogoPrefix 是唯一接受的内联图片前缀。
	siteLogoPrefix = "data:image/"

	// siteLogoBase64Mark 是 data URL 里声明 base64 编码的分隔标记。
	siteLogoBase64Mark = ";base64,"
)

type SiteHandler struct {
	store *store.Store
}

func NewSiteHandler(st *store.Store) *SiteHandler {
	return &SiteHandler{store: st}
}

// siteBrandingResponse 是 GET/PUT 两条端点的响应形状。
//
// 注意这里**只有**站点名称与 LOGO：GET 是免鉴权端点，顺带回显任何其它
// 设置（超时、故障转移策略、默认模型容量）都等于在一个未认证的信息出口上
// 泄漏部署细节。字段名用 site_ 前缀而不是 name/logo，是为了让前端拿到
// 响应时不需要记住「这个 name 是谁的 name」。
// @ts-type SiteBranding
type siteBrandingResponse struct {
	SiteName string `json:"site_name"`
	SiteLogo string `json:"site_logo"`
}

// Get 返回站点品牌；未设置时两个字段都是空串。
//
// 空串而不是后端代填默认名：默认名的选择是前端的事（见 web/src/ui/branding.ts
// 的 DEFAULT_SITE_NAME），后端若也写死一份，改默认名就要同时发前后端两个版本，
// 两边不一致时还会出现「登录页叫 A、侧边栏叫 B」。
//
// 这条端点挂在免鉴权 mux 上（见 internal/api/api.go 的 routeSpecs），
// 因此它必须在**没有会话**时也能 200 —— 登录页的渲染在此之前。
func (h *SiteHandler) Get(w http.ResponseWriter, r *http.Request) {
	b, err := h.store.GetSiteBranding(r.Context())
	if err != nil {
		writeServerError(w, "get site branding", err)
		return
	}
	writeJSON(w, http.StatusOK, siteBrandingResponse{SiteName: b.Name, SiteLogo: b.Logo})
}

// Update 全量覆盖站点品牌。两个字段永远一起提交（见 store.SetSiteBranding）。
//
// 校验全部硬性：
//   - 名称超长会让顶栏挤压布局，前端虽然也截断，但后端不能依赖前端 ——
//     curl 一把就能写进超长值，之后所有页面都跟着变形；
//   - LOGO 只接受 data:image/...;base64,... 内联图片。**不接受远程 URL**
//     有两个理由：远程 URL 会让每个访客的浏览器去请求第三方地址（泄漏
//     访问者 IP，等于给站点装了个追踪器），且把登录页的可渲染性绑在别人家
//     的可用性上 —— 对方挂了，这里连登录框都显示不出来。
func (h *SiteHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req siteBrandingResponse
	if err := decodeJSON(w, r, &req); err != nil {
		if decodeFailed(err) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	name := strings.TrimSpace(req.SiteName)
	if n := utf8.RuneCountInString(name); n > maxSiteNameRunes {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("site_name 过长：%d 个字符，上限 %d 个字符", n, maxSiteNameRunes))
		return
	}

	logo, err := normalizeSiteLogo(req.SiteLogo)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.store.SetSiteBranding(r.Context(), store.SiteBranding{Name: name, Logo: logo}); err != nil {
		writeServerError(w, "save site branding", err)
		return
	}

	// 只回 {"status":"ok"}：前端保存后自己重新拉 GET 取规范化后的值，
	// 比在这里回一份可能与写入值不一致的回显更不容易产生歧义。
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// normalizeSiteLogo 校验并规范化 LOGO 的 data URL。
//
// 空串是合法值，表示「清除 LOGO」（前端回落首字母方块）。返回的错误文案
// 会直接展示给用户，所以每条都写清「哪里不对 + 怎么改」。
func normalizeSiteLogo(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if !strings.HasPrefix(v, siteLogoPrefix) {
		return "", fmt.Errorf(
			"site_logo 必须是 data:image/...;base64,... 形式的内联图片，不接受远程 URL（外部图片会让每个访客的浏览器去请求第三方地址）")
	}
	mark := strings.Index(v, siteLogoBase64Mark)
	if mark < 0 {
		return "", fmt.Errorf("site_logo 缺少 %q 声明，无法确认是 base64 编码的内联图片", siteLogoBase64Mark)
	}
	// 前缀之后必须真的写了图片类型（如 png、jpeg、svg+xml），
	// 否则 "data:image/;base64,..." 这种空 MIME 会被放进去，
	// 浏览器无法判断类型，部分环境直接不渲染。
	if mark == len(siteLogoPrefix) {
		return "", fmt.Errorf("site_logo 缺少图片类型，应形如 data:image/png;base64,...")
	}
	payload := v[mark+len(siteLogoBase64Mark):]
	if payload == "" {
		return "", fmt.Errorf("site_logo 的 base64 内容为空")
	}

	// 先按编码长度上界拦一次，再解码。管理接口的请求体上限是 32MB
	// （config.Defaults.MaxRequestBodyBytes），一个 20MB 的 data URL 能顺利
	// 进到这里；直接 DecodeString 会先吃 15MB 内存才发现超限 —— 而这个
	// 端点只需管理员权限，没必要给一个能顺手打出内存峰值的形状。
	if maxEncoded := base64.StdEncoding.EncodedLen(maxSiteLogoBytes); len(payload) > maxEncoded {
		return "", fmt.Errorf("site_logo 过大：base64 后 %d 个字符，解码后上限 %d 字节（256KB）",
			len(payload), maxSiteLogoBytes)
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("site_logo 的 base64 内容无法解码：%v", err)
	}
	if len(decoded) > maxSiteLogoBytes {
		return "", fmt.Errorf("site_logo 过大：解码后 %d 字节，上限 %d 字节（256KB）",
			len(decoded), maxSiteLogoBytes)
	}
	return v, nil
}
