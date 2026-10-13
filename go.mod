module github.com/yourorg/probe

go 1.22

require (
	golang.org/x/crypto v0.31.0
	golang.org/x/net v0.33.0
	golang.org/x/time v0.8.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.34.4
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.28.0 // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
)

// 依赖策略（对应文档 9.5 / S12）：
//   - 依赖刻意最小化（直接依赖 5 个、合计 18 个模块），CI 运行 govulncheck。
//   - SQLite 使用 modernc.org/sqlite：纯 Go，CGO_ENABLED=0 静态编译。
//   - 首次构建执行：go mod tidy（依赖经 GOPROXY 拉取，见 docs/03 §3.2）。
//   - **vendor/ 不随仓库提交**：go mod vendor 实测 197 MB（modernc.org 占 190 MB），
//     而依赖走 GOPROXY 只有几 MB。需要离线构建时在本机自行 go mod vendor，
//     那份目录已被 .gitignore 排除。
