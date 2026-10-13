# XB-Probe —— 构建与发布（对应文档 12.6）
#
# 所有产物均为 CGO_ENABLED=0 静态编译，不使用 UPX（避免杀软误报）。

MODULE      := github.com/koyoca/xb-probe
VERSION     ?= 1.0.0
LDFLAGS     := -s -w -X main.version=$(VERSION)
BUILD_FLAGS := -trimpath
DIST        := dist/$(VERSION)

# 跨平台 SHA-256：Linux/Git Bash 用 sha256sum，macOS 用 shasum -a 256（输出格式一致）
SHA256SUM   := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || \
                    (command -v shasum >/dev/null 2>&1 && echo "shasum -a 256" || echo ""))

# 发布矩阵（12.4 平台支持矩阵）。
#
# 注意：第 2 段是「产物名里的架构标签」，不是 GOARCH。
#       armv7/armv6 必须用 GOARCH=arm + GOARM=7/6（armv7 不是合法 GOARCH）。
# 用 `go tool dist list` 可确认本工具链支持的全部组合。
#
# 支持范围（2026-10 明确）：**只做 Windows + FreeBSD + Linux**，不含 macOS。
#   - darwin 曾在本矩阵里，但采集器只有 linux/windows/freebsd 三份实现，
#     macOS 会落到 collector_stub.go（采集全空），发出去只会得到"在线但无数据"的僵尸节点。
#     与其发一个假的，不如不发——要支持 macOS 得先有 darwin 采集器。
#   - mips64le 也不能加：Master 依赖的 modernc.org/sqlite 没有该架构的生成代码
#     （缺 sqlite3_index_constraint 等符号），编译必失败。
#   - freebsd/386 与 freebsd/arm 已纳入：if_data 计数在 32 位上是 uint32，
#     采集器按字段宽度读取，编译与运行都成立。
PLATFORMS := linux/amd64 linux/arm64 linux/armv7 linux/armv6 linux/386 linux/riscv64 \
             windows/amd64 windows/386 windows/arm64 \
             freebsd/amd64 freebsd/arm64 freebsd/386 freebsd/arm

# Master 的发布矩阵（比 Agent 窄得多）。
# 只发 Linux 两种架构：Master 是常驻服务端，没有人在 Windows/FreeBSD 上拿它当入口，
# 多编几个平台只是多几份没人下载的产物。
MASTER_PLATFORMS := linux/amd64 linux/arm64

.PHONY: all tidy fmt vet test test-race vulncheck master agent dist master-dist \
        checksums checksums-master release-assets sign verify-sums smoke clean

all: master agent

tidy:
	go mod tidy

fmt:
	gofmt -w -s .

vet:
	go vet ./...

test:
	go test ./...

# 竞态检测需要 cgo 与 C 编译器（gcc / MinGW-w64 / clang）。
test-race:
	CGO_ENABLED=1 go test -race ./...

# 依赖漏洞扫描（S12）：CI 必跑
vulncheck:
	@command -v govulncheck >/dev/null 2>&1 || { \
		echo "未安装 govulncheck，执行：go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	govulncheck ./...

# Master 单二进制：内嵌默认主题与迁移脚本（12.1 / 11）
master:
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/xb-probe-master ./cmd/xb-probe-master

# 本机 Agent
agent:
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/xb-probe-agent ./cmd/xb-probe-agent

# 交叉编译发布产物：xb-probe-agent_<os>_<标签>[.exe]
# 同时把发布标签写进二进制：armv7/armv6 在 Go 里都是 GOARCH=arm（靠 GOARM 区分），
# 运行时拿不到 GOARM，自升级只能靠注入的标签精确匹配产物名（见 internal/dist.CurrentTag）。
dist:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; tag=$${p#*/}; \
		case "$$tag" in \
			armv7) goarch=arm; goarm=7 ;; \
			armv6) goarch=arm; goarm=6 ;; \
			*)     goarch=$$tag; goarm="" ;; \
		esac; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		ldflags="$(LDFLAGS) -X $(MODULE)/internal/dist.PlatformTag=$${os}_$${tag}"; \
		echo ">> $$os/$$tag (GOARCH=$$goarch$${goarm:+ GOARM=}$$goarm)"; \
		if [ -n "$$goarm" ]; then \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$goarch GOARM=$$goarm go build $(BUILD_FLAGS) \
				-ldflags "$${ldflags}" -o "$(DIST)/xb-probe-agent_$${os}_$${tag}$${ext}" ./cmd/xb-probe-agent || exit 1; \
		else \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$goarch go build $(BUILD_FLAGS) \
				-ldflags "$${ldflags}" -o "$(DIST)/xb-probe-agent_$${os}_$${tag}$${ext}" ./cmd/xb-probe-agent || exit 1; \
		fi; \
	done
	@$(MAKE) checksums

# Master 的交叉编译产物：给"无需编译"的一键部署用
# （install-master-bin.sh 直接从 GitHub Releases 取这两份）。
master-dist:
	@mkdir -p $(DIST)
	@for p in $(MASTER_PLATFORMS); do \
		os=$${p%/*}; tag=$${p#*/}; \
		echo ">> xb-probe-master $$os/$$tag"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$tag go build $(BUILD_FLAGS) \
			-ldflags "$(LDFLAGS) -X $(MODULE)/internal/dist.PlatformTag=$${os}_$${tag}" \
			-o "$(DIST)/xb-probe-master_$${os}_$${tag}" ./cmd/xb-probe-master || exit 1; \
	done
	@$(MAKE) checksums-master

# SHA256SUMS（12.6 / 12.8）：Agent 侧的校验和文件。
#
# 只对 xb-probe-agent_* 生成，**绝不能把 xb-probe-master_* 并进来**：
# 这份文件是 Agent 安装脚本 `sha256sum -c` 的输入，里面多两行 Master 产物，
# Agent 那台机器上当然没有这些文件，校验必然 FAILED——而失败原因看起来
# 完全像是"下载被篡改"。Master 单独一份（见 checksums-master）。
checksums:
	@test -n "$(SHA256SUM)" || { echo "找不到 sha256sum / shasum，无法生成校验和"; exit 1; }
	@cd $(DIST) && $(SHA256SUM) xb-probe-agent_* > SHA256SUMS && echo "==> $(DIST)/SHA256SUMS"

# Master 的校验和：与 Agent 那份分开，理由见上面。
checksums-master:
	@test -n "$(SHA256SUM)" || { echo "找不到 sha256sum / shasum，无法生成校验和"; exit 1; }
	@cd $(DIST) && $(SHA256SUM) xb-probe-master_* > SHA256SUMS.master && echo "==> $(DIST)/SHA256SUMS.master"

# 一次产出全部发布产物：Agent 13 平台 + Master 2 平台 + 两份校验和。
# GitHub Actions 的 release 流水线只调这一个目标（见 .github/workflows/release.yml）。
release-assets: dist master-dist

# 用离线私钥签名 SHA256SUMS（6.8）。
#
# ★ 签名必须用**离线私钥**，绝不能把私钥放到构建机/服务器上：
#   `make sign` 只接受 --key 指向的私钥路径，就是为了逼使用者显式指定，
#   而不是让它在仓库里找默认位置（那样迟早有人把密钥提交进去）。
#
# 典型流程（在离线环境或受控的发布机上）：
#   make dist
#   make sign KEY=/media/usb/minisign.key        # 生成 $(DIST)/SHA256SUMS.minisig
#   # 把 $(DIST) 整个上传到发布源；再把公钥填进后台 install.pubkey
#
# 验证（任何一台机器，无需私钥）：
#   make verify-sums PUB=/path/to/minisign.pub
sign:
	@test -n "$(KEY)" || { echo "用法：make sign KEY=<私钥路径> [VERSION=x.y.z]"; exit 1; }
	@command -v minisign >/dev/null 2>&1 || { echo "未安装 minisign"; exit 1; }
	@test -f "$(DIST)/SHA256SUMS" || { echo "找不到 $(DIST)/SHA256SUMS，先跑 make dist"; exit 1; }
	@test -f "$(KEY)" || { echo "找不到私钥 $(KEY)"; exit 1; }
	minisign -S -s "$(KEY)" -m "$(DIST)/SHA256SUMS" \
		-t "timestamp:$$(date -u +%s) file:SHA256SUMS version:$(VERSION)"
	@echo "==> $(DIST)/SHA256SUMS.minisig"

# 用公钥验证签名（发布前的自检，也可在目标机器上跑）。
verify-sums:
	@test -n "$(PUB)" || { echo "用法：make verify-sums PUB=<公钥路径> [VERSION=x.y.z]"; exit 1; }
	@command -v minisign >/dev/null 2>&1 || { echo "未安装 minisign"; exit 1; }
	minisign -V -p "$(PUB)" -m "$(DIST)/SHA256SUMS"
	@case "$$(cd $(DIST) && $(SHA256SUM) -c SHA256SUMS)" in \
		*"FAILED"*) echo "校验和自检失败"; exit 1 ;; \
	esac
	@echo "==> 签名与校验和均正确（$(VERSION)）"

# 端到端冒烟（自建临时实例，端口 18180/18181，跑完自动清理）。
smoke:
	bash scripts/smoke-test.sh

clean:
	rm -rf bin $(DIST)
