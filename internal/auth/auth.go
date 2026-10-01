package auth

import (
	"crypto/sha256"
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

	// 按哈希索引 O(1) 查表，而不是逐 key 常量时间比较（旧实现全量遍历，
	// 与 DESIGN「SHA-256 索引」的口径也不符）。map 查找的耗时确实会泄露
	// 「与存储哈希前缀的匹配程度」，但存储的是高熵 key 的 SHA-256 ——
	// 前缀匹配信息无法反推哈希原像，更无法还原 key 本身，不构成可用的
	// 侧信道。慢哈希（bcrypt 类）同样不必要：key 是网关生成的高熵随机串
	// （DESIGN §6.2），不是用户口令。
	matched := snapshot.Get().KeysByHash[hashHex]
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
