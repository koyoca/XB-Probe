package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/store"
)

// 探测目标「范围」的回归护栏。
//
// 背景（用户上报的需求）：
//
//	4. 节点详情添加专属探测目标那里，可编辑专属探测目标；
//	5. 探测目标页面，编辑专属探测目标，支持范围可多选节点。
//
// 这两条都落在同一个入参 `node_ids` 上，而它的**存在性**本身有语义：
//
//	字段缺省(nil) → 只改参数，作用域不动（节点详情页的「编辑」走这条）
//	[]           → 创建时 = 建一条全局目标；编辑时 = 错误（已有目标不可能没有节点）
//	[1,3,7]      → 为这几个节点各建一条 / 把这套参数同步到这几个节点
//
// 所以三种形态都要有断言。最容易退化的地方是"把字段缺省当成空范围"——
// 表现就是管理员只想改个端口，却顺手把这条目标的作用域清了个干净。

// doJSON 发一个带 JSON body 的请求；给了 pathVal 就把它设成 PathValue。
func doJSON(t *testing.T, h http.HandlerFunc, method, target string,
	body any, pathVal ...string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if len(pathVal) == 2 {
		req.SetPathValue(pathVal[0], pathVal[1])
	}
	w := httptest.NewRecorder()
	h(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func mkNode(t *testing.T, repo *store.Repo, key, name string) int64 {
	t.Helper()
	id, err := repo.CreateNode(key, name, "", time.Now().Unix())
	if err != nil {
		t.Fatalf("建节点 %s 失败：%v", name, err)
	}
	return id
}

// targetOn 找出落在指定节点上的目标（nodeID=0 表示全局）。找不到返回 nil。
func targetOn(t *testing.T, repo *store.Repo, nodeID int64, name string) *model.ProbeTarget {
	t.Helper()
	all, err := repo.ListEveryProbeTarget()
	if err != nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	for _, x := range all {
		if x.NodeID == nodeID && x.Name == name {
			return x
		}
	}
	return nil
}

func countTargets(t *testing.T, repo *store.Repo) int {
	t.Helper()
	all, err := repo.ListEveryProbeTarget()
	if err != nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	return len(all)
}

// ---------------------------------------------------------------------------
// 创建：node_ids 的三态
// ---------------------------------------------------------------------------

func TestCreateTargetScopeStates(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	a := mkNode(t, repo, "pub-a", "A")
	b := mkNode(t, repo, "pub-b", "B")

	base := map[string]any{
		"carrier": "custom", "port": 443, "method": "tcp", "timeout_ms": 2000,
	}

	// ① 多选：给两个节点各建一条同参数目标。
	body := map[string]any{}
	for k, v := range base {
		body[k] = v
	}
	body["node_ids"] = []int64{a, b}
	body["name"] = "专线"
	body["host"] = "10.0.0.1"
	rec, out := doJSON(t, h.handleCreateTarget, http.MethodPost, "/api/admin/v1/probe-targets", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("多选创建应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got, _ := out["created"].(float64); int(got) != 2 {
		t.Errorf("created 应为 2，得到 %v", out["created"])
	}
	if ids, _ := out["ids"].([]any); len(ids) != 2 {
		t.Errorf("ids 应有两个元素，得到 %v", out["ids"])
	}
	if x := targetOn(t, repo, a, "专线"); x == nil {
		t.Error("节点 A 上没有建出专属目标")
	} else if x.Host != "10.0.0.1" {
		t.Errorf("A 上目标的主机应为 10.0.0.1，得到 %q", x.Host)
	}
	if x := targetOn(t, repo, b, "专线"); x == nil {
		t.Error("节点 B 上没有建出专属目标（多选只对第一个节点生效了）")
	}

	// ② 空数组：建一条**全局**目标（node_id=0），不是"没有目标"。
	body2 := map[string]any{}
	for k, v := range base {
		body2[k] = v
	}
	body2["node_ids"] = []int64{}
	body2["name"] = "全国-电信"
	body2["host"] = "114.114.114.114"
	rec, out = doJSON(t, h.handleCreateTarget, http.MethodPost, "/api/admin/v1/probe-targets", body2)
	if rec.Code != http.StatusOK {
		t.Fatalf("空范围创建应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got, _ := out["created"].(float64); int(got) != 1 {
		t.Errorf("空范围应只建 1 条，得到 %v", out["created"])
	}
	if x := targetOn(t, repo, 0, "全国-电信"); x == nil {
		t.Error("空范围没有建出全局目标（node_id 为 NULL 的那条）")
	}

	// ③ 字段缺省：沿用旧前端的单值 node_id。
	body3 := map[string]any{}
	for k, v := range base {
		body3[k] = v
	}
	body3["node_id"] = a
	body3["name"] = "老的单值路径"
	body3["host"] = "223.5.5.5"
	rec, _ = doJSON(t, h.handleCreateTarget, http.MethodPost, "/api/admin/v1/probe-targets", body3)
	if rec.Code != http.StatusOK {
		t.Fatalf("单值 node_id 创建应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if x := targetOn(t, repo, a, "老的单值路径"); x == nil {
		t.Error("旧前端的 node_id 单值路径被破坏了")
	}

	// ④ 不存在的节点必须被拒绝：否则会建出一条谁也拉不到、却占着 ID 的目标。
	body4 := map[string]any{}
	for k, v := range base {
		body4[k] = v
	}
	body4["node_ids"] = []int64{a, 999999}
	body4["name"] = "幽灵节点"
	body4["host"] = "1.1.1.1"
	rec, _ = doJSON(t, h.handleCreateTarget, http.MethodPost, "/api/admin/v1/probe-targets", body4)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("含不存在节点应 400，得到 %d", rec.Code)
	}
	if x := targetOn(t, repo, a, "幽灵节点"); x != nil {
		t.Error("部分节点非法时不应先写入合法的那部分（校验必须在写库之前做完）")
	}

	// ⑤ 全是非法 id（0 / 重复）时不能悄悄退化成全局目标。
	body5 := map[string]any{}
	for k, v := range base {
		body5[k] = v
	}
	body5["node_ids"] = []int64{0, 0}
	body5["name"] = "全非法"
	body5["host"] = "1.1.1.1"
	rec, _ = doJSON(t, h.handleCreateTarget, http.MethodPost, "/api/admin/v1/probe-targets", body5)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("全是非法节点 id 应 400，得到 %d", rec.Code)
	}
	if x := targetOn(t, repo, 0, "全非法"); x != nil {
		t.Error("全非法节点 id 退化成全局目标了：管理员想指定节点却建出了全局目标")
	}
}

// ---------------------------------------------------------------------------
// 编辑：多选范围同步
// ---------------------------------------------------------------------------

// syncBody 组装一次"编辑并同步到这几个节点"的请求体。
func syncBody(name, host string, nodeIDs []int64) map[string]any {
	return map[string]any{
		"name": name, "carrier": "unicom", "host": host,
		"port": 443, "method": "tcp", "timeout_ms": 2000,
		"enabled": true, "show_in_bars": true,
		"node_ids": nodeIDs,
	}
}

func TestPatchTargetScopeSync(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	a := mkNode(t, repo, "pub-a", "A")
	b := mkNode(t, repo, "pub-b", "B")
	c := mkNode(t, repo, "pub-c", "C")

	origin := &model.ProbeTarget{NodeID: a, Name: "专线", Carrier: "unicom",
		Host: "10.1.1.1", Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true}
	id, err := repo.CreateProbeTarget(origin)
	if err != nil {
		t.Fatalf("建初始目标失败：%v", err)
	}
	// 一条同名同运营组的**全局**目标：作用域同步绝不能把它当成"节点上的兄弟"
	// 去覆盖——那会让所有节点一起被改。
	globalID, err := repo.CreateProbeTarget(&model.ProbeTarget{Name: "专线", Carrier: "unicom",
		Host: "10.9.9.9", Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true})
	if err != nil {
		t.Fatalf("建全局同名目标失败：%v", err)
	}

	// ① 把参数同步到 A + B：B 上没有同名目标 → 新建。
	rec, out := doJSON(t, h.handlePatchTarget, http.MethodPatch, "/api/admin/v1/probe-targets/1",
		syncBody("专线", "10.2.2.2", []int64{a, b}), "id", strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("同步到 A+B 应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got, _ := out["created"].(float64); int(got) != 1 {
		t.Errorf("应新建 1 条（B），得到 created=%v", out["created"])
	}
	if got, _ := out["updated"].(float64); int(got) != 0 {
		t.Errorf("不应有更新，得到 updated=%v", out["updated"])
	}
	if x := targetOn(t, repo, a, "专线"); x == nil {
		t.Error("A 上的目标消失了")
	} else if x.Host != "10.2.2.2" {
		t.Errorf("A 上目标的主机应被更新为 10.2.2.2，得到 %q", x.Host)
	}
	if x := targetOn(t, repo, b, "专线"); x == nil {
		t.Error("B 上没有新建出目标")
	} else if x.Host != "10.2.2.2" {
		t.Errorf("B 上目标的主机应为 10.2.2.2，得到 %q", x.Host)
	}
	if x := targetOn(t, repo, 0, "专线"); x == nil {
		t.Error("全局的同名目标被删掉了")
	} else if x.Host != "10.9.9.9" {
		t.Errorf("全局的同名目标被覆盖了（%q）——作用域同步误伤了全局目标", x.Host)
	}

	before := countTargets(t, repo)

	// ② 再加一个节点：B 上已有兄弟目标 → 原地更新，不重复新建。
	rec, out = doJSON(t, h.handlePatchTarget, http.MethodPatch, "/api/admin/v1/probe-targets/1",
		syncBody("专线", "10.3.3.3", []int64{a, b, c}), "id", strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("同步到 A+B+C 应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got, _ := out["created"].(float64); int(got) != 1 {
		t.Errorf("第二次应只新建 1 条（C），得到 created=%v", out["created"])
	}
	if got, _ := out["updated"].(float64); int(got) != 1 {
		t.Errorf("第二次应更新 1 条（B），得到 updated=%v", out["updated"])
	}
	if got := countTargets(t, repo); got != before+1 {
		t.Errorf("重复同步造出了多余目标：%d → %d（期望只 +1）", before, got)
	}
	if x := targetOn(t, repo, b, "专线"); x == nil {
		t.Error("B 上的目标丢了")
	} else if x.Host != "10.3.3.3" {
		t.Errorf("B 上目标应被更新为 10.3.3.3，得到 %q", x.Host)
	}
	if x := targetOn(t, repo, c, "专线"); x == nil {
		t.Error("C 上没有新建出目标")
	}

	// ③ 反复同步不能每次多建一条（命中多条同名目标时取 id 最小的那条）。
	rec, out = doJSON(t, h.handlePatchTarget, http.MethodPatch, "/api/admin/v1/probe-targets/1",
		syncBody("专线", "10.3.3.3", []int64{a, b, c}), "id", strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("重复同步应 200，得到 %d", rec.Code)
	}
	if got, _ := out["created"].(float64); int(got) != 0 {
		t.Errorf("幂等同步不应再新建，得到 created=%v", out["created"])
	}

	if x := targetOn(t, repo, 0, "专线"); x == nil || x.ID != globalID {
		t.Error("全局目标在多次同步后被动过")
	}
}

// errMsg 取出错误响应里的 message（writeErr 的封装形状）。
func errMsg(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

func TestPatchTargetScopeRejections(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	a := mkNode(t, repo, "pub-a", "A")
	b := mkNode(t, repo, "pub-b", "B")

	id, err := repo.CreateProbeTarget(&model.ProbeTarget{NodeID: a, Name: "专线", Carrier: "unicom",
		Host: "10.1.1.1", Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true})
	if err != nil {
		t.Fatalf("建目标失败：%v", err)
	}
	globalID, err := repo.CreateProbeTarget(&model.ProbeTarget{Name: "全局", Carrier: "mobile",
		Host: "223.5.5.5", Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true})
	if err != nil {
		t.Fatalf("建全局目标失败：%v", err)
	}
	before := countTargets(t, repo)

	// ① 空数组：一条已有目标不可能"没有节点"，必须报错而不是静默 no-op。
	//
	// 断言的是**具体那句提示**：这一条其实是"防御性"的——就算删掉它，
	// 下面的 ② 也会拦下来（空范围会被折成全局，而全局又不在自身节点上）。
	// 但那时管理员看到的是"不能把目标移出它自己的节点"，完全指不到问题所在，
	// 所以这里把它锁在"范围不能为空"上。
	rec, out := doJSON(t, h.handlePatchTarget, http.MethodPatch, "/", syncBody("专线", "10.2.2.2", []int64{}), "id", strconv.FormatInt(id, 10))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空范围应 400，得到 %d", rec.Code)
	}
	if m := errMsg(out); !strings.Contains(m, "范围不能为空") {
		t.Errorf("空范围的提示应说明「范围不能为空」，得到 %q", m)
	}

	// ② 把目标移出它自己的节点：会留下孤儿延迟桶。
	rec, out = doJSON(t, h.handlePatchTarget, http.MethodPatch, "/", syncBody("专线", "10.2.2.2", []int64{b}), "id", strconv.FormatInt(id, 10))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("把目标移出自身节点应 400，得到 %d", rec.Code)
	}
	if m := errMsg(out); !strings.Contains(m, "勾选") {
		t.Errorf("移出自身节点的提示应告诉管理员把节点勾回来，得到 %q", m)
	}

	// ③ 节点不存在。
	rec, _ = doJSON(t, h.handlePatchTarget, http.MethodPatch, "/", syncBody("专线", "10.2.2.2", []int64{a, 999999}), "id", strconv.FormatInt(id, 10))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("含不存在节点应 400，得到 %d", rec.Code)
	}

	// ④ 全局目标不能被收窄成专属。
	// 同样：真正的兜底是 ② 那条（0 永远不会出现在范围里），但提示必须是
	// "全局目标不可收窄"，否则管理员会以为是自己勾错了节点。
	rec, out = doJSON(t, h.handlePatchTarget, http.MethodPatch, "/", syncBody("全局", "10.2.2.2", []int64{a}), "id", strconv.FormatInt(globalID, 10))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("收窄全局目标应 400，得到 %d", rec.Code)
	}
	if m := errMsg(out); !strings.Contains(m, "全局目标") {
		t.Errorf("收窄全局目标的提示应提到「全局目标」，得到 %q", m)
	}

	// 上面每一次都必须"被拒绝且什么都没改"。
	if got := countTargets(t, repo); got != before {
		t.Errorf("被拒绝的请求改动了数据：目标数 %d → %d", before, got)
	}
	if x := targetOn(t, repo, a, "专线"); x == nil {
		t.Error("A 上的目标丢了")
	} else if x.Host != "10.1.1.1" || x.Carrier != "unicom" {
		t.Errorf("被拒绝的作用域请求仍然把参数写进去了：host=%q carrier=%q", x.Host, x.Carrier)
	}
	if x := targetOn(t, repo, b, "专线"); x != nil {
		t.Error("被拒绝的请求仍然在 B 上建了目标")
	}
	if x := targetOn(t, repo, 0, "全局"); x == nil {
		t.Error("全局目标丢了")
	} else if x.NodeID != 0 {
		t.Errorf("全局目标的作用域被改了：node_id=%d", x.NodeID)
	}
}

// 节点详情页的「编辑」不带 node_ids：只改参数，作用域绝不能被动。
func TestPatchTargetWithoutScopeLeavesScopeAlone(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	a := mkNode(t, repo, "pub-a", "A")
	b := mkNode(t, repo, "pub-b", "B")

	id, err := repo.CreateProbeTarget(&model.ProbeTarget{NodeID: a, Name: "专线", Carrier: "unicom",
		Host: "10.1.1.1", Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true})
	if err != nil {
		t.Fatalf("建目标失败：%v", err)
	}
	before := countTargets(t, repo)

	rec, _ := doJSON(t, h.handlePatchTarget, http.MethodPatch, "/",
		map[string]any{"name": "改名了", "port": 8443}, "id", strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("不带范围的编辑应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got := countTargets(t, repo); got != before {
		t.Errorf("不提交 node_ids 却改了目标数量：%d → %d", before, got)
	}
	if x := targetOn(t, repo, b, "改名了"); x != nil {
		t.Error("不提交 node_ids 却把目标复制到了别的节点")
	}
	got := targetOn(t, repo, a, "改名了")
	if got == nil {
		t.Fatal("改名没生效")
	}
	if got.NodeID != a {
		t.Errorf("作用域被改了：node_id=%d，应为 %d", got.NodeID, a)
	}
	if got.Port != 8443 {
		t.Errorf("端口没改成 8443，得到 %d", got.Port)
	}
	// 没提交的字段保持原值（PATCH 语义）。
	if got.Carrier != "unicom" || got.Host != "10.1.1.1" {
		t.Errorf("未提交的字段被清掉了：carrier=%q host=%q", got.Carrier, got.Host)
	}
}
