package dist

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 发布产物的"名字、平台集合、校验和文件名"在三个地方各写了一遍：
//
//	· Makefile                        —— 真正把产物编出来的地方
//	· .github/workflows/release.yml   —— 把产物发到 Releases 的地方
//	· install-master-bin.sh           —— 在用户机器上按名字把它们取回来的地方
//
// 三处只要有一处漂了，症状都会出现在**用户那一端**：下载 404、或者
// "校验和不匹配"（而后者看起来像被篡改）。仓库里却一切正常、CI 全绿——
// 因为 CI 只验"能不能编出来"，不验"编出来的名字与取件方期望的是否一致"。
//
// 这里的做法是把三个文件读回来逐项对齐，而不是把常量抄一遍：
// 抄一遍等于多了一处要同步的地方，护栏本身就成了新的漂移源。

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("读不到 %s：%v（判据已与仓库布局脱节）", rel, err)
	}
	return string(raw)
}

// parseMakeList 取出 `NAME := a b c` 的值，并接上反斜杠续行。
//
// 刻意"逐行找 + 手工拼续行"，不用一条正则吃掉多行。写成
// “^NAME[ \t]*:=[ \t]*(.*(?:\\\n.*)*)“ 看着很整齐，但在 Go 的 regexp 里
// 贪婪的 `.*` 会把行尾那个续行反斜杠也吃掉，而可选的重复组一次都不匹配就收工：
// 结果是**静默只解析到第一行，还多出一个 `\` 字段**。这不是假设——13 个平台
// 被解析成 7 个，直到数量断言报出来才看见；而"少一半平台"如果没人断言，
// 就会一路漂到用户那里变成 404。
func parseMakeList(t *testing.T, makefile, name string) []string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, name) {
			continue
		}
		rest := strings.TrimLeft(strings.TrimPrefix(line, name), " \t")
		if !strings.HasPrefix(rest, ":=") {
			continue // 同前缀的其它变量，或注释里的提及
		}
		value := strings.TrimSpace(strings.TrimPrefix(rest, ":="))
		for strings.HasSuffix(value, "\\") {
			value = strings.TrimSpace(strings.TrimSuffix(value, "\\"))
			i++
			if i >= len(lines) {
				t.Fatalf("%s 的续行读到了文件末尾", name)
			}
			value += " " + strings.TrimSpace(lines[i])
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			t.Fatalf("%s 解析出 0 个平台——判据实际上没在检查任何东西", name)
		}
		return fields
	}
	t.Fatalf("Makefile 里找不到 %s 的定义——判据已与 Makefile 脱节", name)
	return nil
}

// makeDistPrefixes 取出 Makefile 里所有 `-o "$(DIST)/<前缀>_$${os}_$${tag}…"`
// 形式的产物名前缀，用于和 Go 侧、脚本侧对齐。
func makeDistPrefixes(t *testing.T, makefile string) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`-o "\$\(DIST\)/([a-z-]+)_\$\$\{os\}_\$\$\{tag\}`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(makefile, -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("Makefile 里一个产物名模式都没抽到——判据已与 Makefile 写法脱节")
	}
	return out
}

func TestReleaseArtifactNamingIsConsistentEverywhere(t *testing.T) {
	mk := readRepoFile(t, "../../Makefile")
	inst := readRepoFile(t, "../../install-master-bin.sh")
	wf := readRepoFile(t, "../../.github/workflows/release.yml")

	agentPlatforms := parseMakeList(t, mk, "PLATFORMS")
	masterPlatforms := parseMakeList(t, mk, "MASTER_PLATFORMS")

	prefixes := makeDistPrefixes(t, mk)

	// --- 1) 产物名前缀：Go 代码 ↔ Makefile ---
	// ArtifactName 是 Agent 侧自升级计算"该下载哪个文件"的唯一来源，
	// 它必须与 Makefile 真正写出的文件名同前缀。
	agentPrefix := strings.SplitN(ArtifactName("linux", "amd64"), "_", 2)[0]
	if !prefixes[agentPrefix] {
		t.Errorf("Makefile 里没有 %q 前缀的产物（抽到的是 %v）——"+
			"ArtifactName 算出的名字与实际编出来的文件对不上，Agent 自升级会 404",
			agentPrefix, keysOf(prefixes))
	}
	const masterPrefix = "probe-master"
	if !prefixes[masterPrefix] {
		t.Errorf("Makefile 里没有 Master 产物（抽到的是 %v）——免编译部署取不到二进制", keysOf(prefixes))
	}

	// --- 2) 免编译脚本期望的名字 ↔ Makefile ---
	// 脚本里写的是 probe-master_linux_${ARCH}；把它按主机架构展开后，
	// 必须恰好是 Makefile 会编出来的那些文件名。
	reInstName := regexp.MustCompile(`BIN_NAME="([a-z-]+)_linux_\$\{ARCH\}"`)
	im := reInstName.FindStringSubmatch(inst)
	if im == nil {
		t.Fatal("install-master-bin.sh 里找不到 BIN_NAME 模板——判据已与脚本写法脱节")
	}
	if im[1] != masterPrefix {
		t.Errorf("免编译脚本取的是 %q_linux_…，而 Makefile 编的是 %q_linux_…——"+
			"两边前缀不一致，用户下载必然 404", im[1], masterPrefix)
	}

	// --- 3) 架构集合：脚本认的架构 ↔ Makefile 的 Master 平台 ---
	if im[1] == "" {
		t.Fatal("产物名前缀为空")
	}
	reArch := regexp.MustCompile(`\)[ \t]*ARCH="([a-z0-9]+)"`)
	var instArch []string
	for _, m := range reArch.FindAllStringSubmatch(inst, -1) {
		instArch = append(instArch, m[1])
	}
	if len(instArch) == 0 {
		t.Fatal("没从免编译脚本里抽到任何架构分支——判据已与脚本写法脱节")
	}
	var mkMasterArch, mkMasterOS []string
	for _, p := range masterPlatforms {
		parts := strings.SplitN(p, "/", 2)
		if len(parts) != 2 {
			t.Fatalf("MASTER_PLATFORMS 里的 %q 不是 os/arch 形式", p)
		}
		mkMasterOS = append(mkMasterOS, parts[0])
		mkMasterArch = append(mkMasterArch, parts[1])
	}
	sort.Strings(instArch)
	sort.Strings(mkMasterArch)
	if strings.Join(instArch, ",") != strings.Join(mkMasterArch, ",") {
		t.Errorf("免编译脚本认的架构是 [%s]，Makefile 发布的 Master 架构是 [%s]——"+
			"差集里的那种架构，用户装了会在下载那一步失败（脚本本身却看不出问题）",
			strings.Join(instArch, ","), strings.Join(mkMasterArch, ","))
	}
	for _, osName := range mkMasterOS {
		// 脚本把产物名写死成 linux_：哪天 Master 发布矩阵加了别的 OS，
		// 这里的断言会立刻拦下来。
		if osName != "linux" {
			t.Errorf("Master 平台 %s 不是 linux，但免编译脚本的产物名写死了 linux——两边已不一致", osName)
		}
	}

	// --- 4) 校验和文件名：Makefile 生成的 ↔ 脚本下载的 ---
	reSumMk := regexp.MustCompile(`> (SHA256SUMS[a-z.]*)`)
	sm := reSumMk.FindAllStringSubmatch(mk, -1)
	var mkSums []string
	for _, m := range sm {
		mkSums = append(mkSums, m[1])
	}
	if len(mkSums) != 2 {
		t.Fatalf("Makefile 应生成两份校验和（Agent 一份、Master 一份），实际 %v——"+
			"合成一份会让 Agent 侧 sha256sum -c 找不到 Master 文件而报 FAILED", mkSums)
	}
	// 脚本要的必须正是 Master 那一份，而不是 Agent 的 SHA256SUMS。
	if !strings.Contains(inst, `$TAG/SHA256SUMS.master`) {
		t.Errorf("免编译脚本没有下载 Master 的校验和文件（Makefile 生成的是 %v）；"+
			"取错文件会以「校验和不匹配」的形式失败，看起来像下载被篡改", mkSums)
	}
	if !strings.Contains(mk, "> SHA256SUMS.master") {
		t.Errorf("Makefile 没有生成 SHA256SUMS.master（生成的是 %v）", mkSums)
	}

	// --- 5) 发布流水线的数量断言 ↔ Makefile 的平台清单 ---
	// 数量写死在 workflow 里是有意的（"少编了一个平台"必须失败），
	// 代价就是加平台时必须同步改数字——这条断言把这个代价变成一次编译期提醒。
	if !strings.Contains(mk, "release-assets: dist master-dist") {
		t.Error("Makefile 的 release-assets 没有依赖 master-dist——" +
			"发布流水线会只发 Agent，免编译部署随即取不到 Master 二进制")
	}
	if !strings.Contains(wf, "make release-assets") {
		t.Error("发布流水线没有调用 make release-assets（构建定义必须只有一处）")
	}
	for _, c := range []struct {
		what     string
		platform []string
	}{
		{"Agent", agentPlatforms},
		{"Master", masterPlatforms},
	} {
		if !strings.Contains(wf, "-eq "+strconv.Itoa(len(c.platform))) {
			t.Errorf("发布流水线里没有 -eq %d 这条%s数量断言（解析到的平台：%s）；"+
				"少了它就会出现「少发一个平台的产物而没人知道」",
				len(c.platform), c.what, strings.Join(c.platform, " "))
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
