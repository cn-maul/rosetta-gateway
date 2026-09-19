package webui

import "embed"

// dist 由 web/ 前端工程构建后经 web/sync-embed.mjs 同步而来。
// 构建流程：cd web && npm run build && npm run sync
// all: 前缀确保以下划线开头的文件也被嵌入。
//
//go:embed all:dist
var StaticFS embed.FS
