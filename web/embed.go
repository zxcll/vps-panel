// Package web 把前端资源打进二进制。
//
// 前端刻意不用打包器：源码就是浏览器直接能跑的 ES 模块，
// 加上一份 Vue 运行时。这样 `go build` 一条命令就能产出完整可用的面板，
// 部署方和二次开发的人都不需要装 Node。
package web

import "embed"

//go:embed index.html style.css assets vendor
var FS embed.FS
