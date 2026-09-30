package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

type Context struct {
	KeyID string
	Name  string
}

func Authenticate(r *http.Request) (*Context, error) {
	key := extractKey(r)
	if key == "" {
		return nil, ErrNoKey
	}

	hash := sha256.Sum256([]byte(key))
	hashHex := hex.EncodeToString(hash[:])

	// 遍历**全部** key、把结果累积起来再判定，而不是命中即 return：
	//   - `==` 字符串比较是逐字节短路，响应时间会泄露「前缀对了几个字节」；
	//   - 命中即返回还会让耗时随「匹配项在 map 遍历顺序中的位置」浮动。
	// 两者叠加就是一条可用的计时侧信道（本机、无网络抖动时尤其好利用）。
	// 遍历的总项数固定，用 ConstantTimeCompare 逐项比较，时间与「匹配到哪一项」无关。
	snap := snapshot.Get()
	var matched *snapshot.KeySnapshot
	for _, ks := range snap.Keys {
		if subtle.ConstantTimeCompare([]byte(ks.KeyHash), []byte(hashHex)) == 1 {
			matched = ks
		}
	}

	if matched == nil {
		return nil, ErrInvalidKey
	}
	if !matched.Enabled {
		return nil, ErrKeyDisabled
	}
	return &Context{KeyID: matched.ID, Name: matched.Name}, nil
}

// extractKey 只从请求头取密钥。
//
// 刻意不支持 `?key=`：查询串会被写进访问日志、Referer、以及沿途中间代理的日志，
// 等于把一把长期有效的密钥散落到多个不设防的地方。Authorization / X-Api-Key
// 是唯一入口。
func extractKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return auth[len(prefix):]
	}

	if key := r.Header.Get("X-Api-Key"); key != "" {
		return key
	}

	return ""
}
