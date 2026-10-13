package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/probe/internal/auth"
	"github.com/yourorg/probe/internal/dist"
	"github.com/yourorg/probe/internal/model"
)

// Agent 自升级的取件接口（12.6 的客户端侧）。
//
// 为什么不复用公开的 /dist/：
// `install.mode` 默认是 on_demand，只有"存在未使用的注册码"时才开放 /dist/。
// 节点装好之后注册码就被消费了，那条路随即关闭——照它的语义这是对的
// （不该长期匿名分发二进制），但升级就被一起关死了。
//
// 所以升级走 **agent 作用域**：用节点令牌鉴权，一个已注册节点随时可以取
// 自己平台的构件。这样既不需要长期开放匿名分发，也不会把注册码的语义改成
// "注册码没用完就一直开着"。
//
// 两个接口都只认令牌，不认"这台机器是不是管理员"：升级的是 Agent 自己，
// 它拿不到也不需要管理员会话。

// agentRelease 是"有没有新版本"的答案。
type agentRelease struct {
	Version  string `json:"version"`
	Artifact string `json:"artifact"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Current  string `json:"current"`
	UpToDate bool   `json:"up_to_date"`
}

// handleAgentReleaseLatest 对应 GET /api/agent/v1/releases/latest。
//
// 查询参数：
//   - platform（必填）：发布标签，形如 linux_amd64 / windows_amd64 / linux_armv7。
//     由 Agent 在构建时用 -X 注入，armv7/armv6 才能与产物名精确对应。
//   - current（可选）：Agent 自己的版本号，用于判断"是否已是最新"。
//
// 返回 200 + 最新版本的描述；没有任何已发布版本时返回 404（Agent 据此
// 提示"Master 上还没有发布文件"，而不是当作网络错误去重试）。
func (h *Handler) handleAgentReleaseLatest(w http.ResponseWriter, r *http.Request) {
	_, ok := h.authAgent(w, r)
	if !ok {
		return
	}
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if platform == "" {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "缺少 platform")
		return
	}
	if strings.Contains(platform, "/") || strings.Contains(platform, "..") || strings.Contains(platform, `\`) {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "platform 非法")
		return
	}
	if h.DistDir == "" {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "Master 未配置分发目录")
		return
	}

	version, err := latestDistVersion(h.DistDir)
	if err != nil || version == "" {
		writeErr(w, http.StatusNotFound, model.ErrNotFound,
			"Master 上还没有任何已发布版本（先执行 probe-master dist pull）")
		return
	}

	name := artifactForTag(platform)
	sums, err := readDistSums(h.DistDir, version)
	if err != nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "该版本缺少 SHA256SUMS")
		return
	}
	hash, ok := sums[name]
	if !ok {
		// 明确区分两种"没有"：整个版本没发布 vs 这个平台没发布。
		// 混在一起会让 armv6 节点以为"Master 没配好"，其实是没给它编。
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "该平台未发布此版本："+platform)
		return
	}
	full := filepath.Join(h.DistDir, version, name)
	st, err := os.Stat(full)
	if err != nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "构件文件缺失："+name)
		return
	}

	current := strings.TrimSpace(r.URL.Query().Get("current"))
	writeJSON(w, http.StatusOK, agentRelease{
		Version:  version,
		Artifact: name,
		SHA256:   hash,
		Size:     st.Size(),
		Current:  current,
		// 比最新还新（比如 Master 回滚了版本）也算"不需要动"——
		// 自升级绝不主动降级，降级只能由运维显式指定版本。
		UpToDate: compareVersion(current, version) >= 0,
	})
}

// handleAgentArtifact 对应 GET /api/agent/v1/artifact。
//
// 查询参数：platform（必填）、version（可选，默认最新）。
// 响应头带上 X-Checksum-Sha256，Agent 下载完可立即比对；
// 真正的信任锚是 SHA256SUMS（以及它的 minisign 签名），这个头只用于快速失败。
func (h *Handler) handleAgentArtifact(w http.ResponseWriter, r *http.Request) {
	_, ok := h.authAgent(w, r)
	if !ok {
		return
	}
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if platform == "" {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "缺少 platform")
		return
	}
	if strings.Contains(platform, "/") || strings.Contains(platform, "..") || strings.Contains(platform, `\`) {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "platform 非法")
		return
	}

	version := strings.TrimSpace(r.URL.Query().Get("version"))
	if version == "" {
		v, err := latestDistVersion(h.DistDir)
		if err != nil || v == "" {
			writeErr(w, http.StatusNotFound, model.ErrNotFound, "Master 上还没有任何已发布版本")
			return
		}
		version = v
	}
	if strings.Contains(version, "/") || strings.Contains(version, "..") {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "version 非法")
		return
	}

	sums, err := readDistSums(h.DistDir, version)
	if err != nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "该版本缺少 SHA256SUMS")
		return
	}
	name := artifactForTag(platform)
	hash, ok := sums[name]
	if !ok {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "该平台未发布此版本："+platform)
		return
	}

	full, err := safeDistPath(h.DistDir, filepath.Join(version, name))
	if err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "路径非法")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "文件不存在")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "文件不存在")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Checksum-Sha256", hash)
	w.Header().Set("X-Artifact-Version", version)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// authAgent 校验节点令牌，返回节点 ID；失败时已经写好响应。
func (h *Handler) authAgent(w http.ResponseWriter, r *http.Request) (int64, bool) {
	token, ok := auth.BearerToken(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, model.ErrInvalidToken, "缺少令牌")
		return 0, false
	}
	nodeID, err := h.Tokens.Authenticate(token, RealIP(r), time.Now())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, model.ErrInvalidToken, "令牌无效")
		return 0, false
	}
	return nodeID, true
}

// artifactForTag 把发布标签（windows_amd64 / linux_armv7）转成构件名。
func artifactForTag(tag string) string {
	osName := tag
	if i := strings.Index(tag, "_"); i > 0 {
		osName = tag[:i]
		return dist.ArtifactName(osName, tag[i+1:])
	}
	return dist.ArtifactName(osName, "")
}

// readDistSums 读取某个版本的 SHA256SUMS。
//
// 它是校验的信任根，所以要求**必须存在**：宁可让升级直接失败（明确），
// 也不能在没有校验和的情况下把二进制发出去（一次污染、全部受害）。
func readDistSums(distDir, version string) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(distDir, version, "SHA256SUMS"))
	if err != nil {
		return nil, err
	}
	sums := dist.ParseSums(raw)
	if len(sums) == 0 {
		return nil, errors.New("SHA256SUMS 为空或全部非法")
	}
	return sums, nil
}

// latestDistVersion 在 data_dir/dist 下挑出最新版本号。
//
// 只认"纯数字点分"的目录名（1.2.3），其它名字（比如手工拷进去的 nightly-xxx）
// 一律忽略——版本号一旦出现非数字成分，"哪个更新"就没有客观答案了，
// 而挑错版本的后果是全网 Agent 一起换到一个没打算发的二进制。
func latestDistVersion(distDir string) (string, error) {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return "", err
	}
	best := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !isVersionName(name) {
			continue
		}
		if best == "" || compareVersion(name, best) > 0 {
			best = name
		}
	}
	return best, nil
}

// isVersionName 判定是否是"纯数字点分"的版本号。
func isVersionName(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return false
			}
		}
	}
	return true
}

// compareVersion 比较两个点分版本号，a>b 返回 1，相等 0，a<b 返回 -1。
//
// 按段逐个当整数比，而不是字符串比：字符串比会让 "1.10.0" < "1.9.0"，
// 于是 1.10 发布后 Agent 反而"已经是最新"了。
func compareVersion(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}
