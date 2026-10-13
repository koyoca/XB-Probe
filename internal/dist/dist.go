// Package dist 是"发布构件"的单一事实来源：构件命名、校验和文本的解析、
// 以及文件哈希的计算。
//
// 为什么要单独一个包：这套约定同时被两端使用——
//
//   - Master 的 `xb-probe-master dist pull`（把一整个版本的发布文件拉进 data_dir/dist）
//   - Agent 的 `xb-probe-agent upgrade`（只取自己那一个二进制）
//
// 两端各自写一遍解析的话，必然出现"一边收紧了规则、另一边没有"的漂移，
// 而这类漂移的表现恰好是"校验通过"的假象（比如把非十六进制的行也当成校验和）。
package dist

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PlatformTag 是本机构件的"发布标签"，形如 linux_amd64 / windows_amd64 /
// linux_armv7。由 Makefile 在交叉编译时用
//
//	-ldflags "-X github.com/koyoca/xb-probe/internal/dist.PlatformTag=<os>_<tag>"
//
// 注入。
//
// 为什么必须注入而不是运行时推断：产物名的第 2 段是**标签**而不是 GOARCH——
// armv7/armv6 在 Go 里都是 `GOARCH=arm`（靠 GOARM 区分），运行时拿不到 GOARM，
// 猜错就会下载一个跑不起来的二进制。发布构建时标签是精确已知的，所以直接写进去。
var PlatformTag = ""

// CurrentTag 返回本机的发布标签。
//
// 没有注入时（比如 `make agent` 本机构建）退回 `runtime.GOOS + "_" + runtime.GOARCH`。
// 这个回退在 armv7/armv6 上会得到 `linux_arm`，而发布矩阵里没有这个名字——
// 于是服务端返回"该平台未发布此版本"，而不是装上一个错的二进制。
// 静默猜一个能跑的架构比明确报错更危险，所以这里不做 GOARM 猜测。
func CurrentTag() string {
	if PlatformTag != "" {
		return PlatformTag
	}
	return runtime.GOOS + "_" + runtime.GOARCH
}

// ArtifactName 返回构件文件名，形如 xb-probe-agent_linux_amd64（Windows 带 .exe）。
//
// 与 Makefile 的 dist 目标保持一致：xb-probe-agent_<os>_<tag>[.exe]。
func ArtifactName(osName, tag string) string {
	name := "xb-probe-agent_" + osName + "_" + tag
	if osName == "windows" {
		name += ".exe"
	}
	return name
}

// SelfArtifactName 返回本机构件的文件名。
func SelfArtifactName() string {
	tag := CurrentTag()
	osName := tag
	if i := strings.Index(tag, "_"); i > 0 {
		osName = tag[:i]
	}
	return ArtifactName(osName, tag[len(osName)+1:])
}

// ParseSums 解析 SHA256SUMS 文本（"<hash>  <file>" 格式，兼容 sha256sum -c）。
//
// 两道过滤不能省：
//   - **必须恰好 64 个十六进制字符**。少了这一条，文件里任何 "<词> <词>" 的行
//     （比如注释忘了加 `#`、或源站塞了别的内容）都会被当成一条有效校验和，
//     结果是"校验通过"的假象——而它其实什么都没校验。
//   - **文件名走 filepath.Base**。SHA256SUMS 正常只有裸文件名，但若上游写成
//     `./xb-probe-agent_linux_amd64` 或带目录前缀，不归一化就会匹配不上任何下载文件。
func ParseSums(body []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 兼容 "<hash>  <file>" 与 "<hash> *<file>"（binary 模式）两种写法。
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		name := strings.TrimPrefix(f[1], "*")
		hash := strings.ToLower(f[0])
		if len(hash) != 64 || !isHex(hash) {
			continue // 不是 sha256
		}
		out[filepath.Base(name)] = hash
	}
	return out
}

// isHex 判定是否全为十六进制字符（防止 "zzzz..." 这种长度凑够 64 的假校验和）。
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// SHA256File 计算文件的 SHA-256（小写十六进制）。
//
// 流式读取而不是一次性读入内存：Agent 要校验的是自己整份二进制，
// 在内存只有几十 MB 的小机器上一次性 ReadFile 很容易把内存顶满，
// 而升级恰恰是最不能把节点搞挂的操作。
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
