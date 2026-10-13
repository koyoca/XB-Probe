// Package tzdata 把 IANA 时区数据库编译进二进制，供主程序空导入即可生效。
//
// 为什么必须有这个包（不是"为了兼容而兼容"）：
//
//  1. Windows 没有 /usr/share/zoneinfo。Go 的 time.LoadLocation 在 Windows 上只
//     会去看 %ZONEINFO%、$GOROOT/lib/time/zoneinfo.zip 与少数几个系统路径；
//     本项目用 -trimpath 静态编译后只分发一个 exe，GOROOT 并不存在，
//     于是 time.LoadLocation("Asia/Shanghai") 必然失败。
//  2. Alpine / distroless / scratch 镜像默认不装 tzdata 包，同样没有 zoneinfo。
//
// 缺少它时的真实表现（不是理论上会坏）：
//
//   - PUT /api/admin/v1/nodes/{id}/billing 只要带 tz 就直接 400「时区名称非法」，
//     而计费保存是一个整表替换，于是额度、到期、口径一起写不进去；
//   - PUT /api/admin/v1/settings 的 timezone 项同样被拒；
//   - traffic.LocalDay / traffic.Tracker 里的 LoadLocation 失败后会静默回退到
//     time.Local，"今日流量"在跨时区部署时算到错误的自然日。
//
// 代价约 450 KB（压缩后更小），而少一次线上排查的价值远高于此。本项目所有产物
// 都是 CGO_ENABLED=0 的静态二进制、面向"单文件丢上去就能跑"的分发形态，
// 因此这个代价是必须付的。
//
// 使用方式（只有 Master 需要，Agent 不引用 internal/traffic，
// 也不做任何本地时区换算，因此不给 Agent 增加体积）：
//
//	import _ "github.com/yourorg/probe/internal/tzdata"
package tzdata

import _ "time/tzdata"
