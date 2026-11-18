// Package web 内嵌前端构建产物（vite build 输出于 web/dist）。
// 构建流程：cd web && npm install && npm run build，再 go build（见 CLAUDE.md）。
package web

import "embed"

// Dist 前端静态文件（根为 dist 内容：index.html 与 assets/）。
//
//go:embed all:dist
var Dist embed.FS
