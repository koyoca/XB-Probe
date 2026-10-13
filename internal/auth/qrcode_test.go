package auth

import (
	"strings"
	"testing"
)

// 二维码编码器的回归测试（6.2 的 TOTP 绑定依赖它）。
//
// 为什么这些用例是"逐模块精确比对"而不是"看起来像就行"：
// 这个编码器在开发过程中出现过**四类**都会让码"能生成、能显示、
// 但扫描器完全解不出来"的错误——
//  1. 分块表把"整版本数据码字总数"当成"每块数"（v4 起全错）
//  2. 格式信息位序写反（v1 起全错）
//  3. 版本信息两块的行列写反（只有 v7+ 错）
//  4. 掩码靠 reserved 判断，结果恰好跳过了所有数据格（等于没打掩码）
// 每一个都不会让矩阵尺寸、寻像图案出问题，肉眼与"能否生成"都发现不了。
// 因此这里的期望值来自独立参考实现（python-qrcode）的逐模块输出。

// qrGolden 参见下方内嵌数据（由参考实现生成）。

func TestQRMatrixMatchesReference(t *testing.T) {
	for _, c := range qrGolden {
		m, err := QRMatrix(c.text)
		if err != nil {
			t.Fatalf("编码 %q 失败：%v", c.text, err)
		}
		size := len(m)
		if want := c.ver*4 + 17; size != want {
			t.Errorf("%q 尺寸 %d，期望 %d（版本 %d）", c.text, size, want, c.ver)
			continue
		}
		if len(c.rows) != size {
			t.Fatalf("%q golden 行数 %d 与尺寸 %d 不符", c.text, len(c.rows), size)
		}
		bad := 0
		for r := 0; r < size; r++ {
			for col := 0; col < size; col++ {
				want := (c.rows[r]>>uint(size-1-col))&1 == 1
				if m[r][col] != want {
					if bad < 8 {
						t.Errorf("%q (%d,%d)：得到 %v，期望 %v", c.text, r, col, m[r][col], want)
					}
					bad++
				}
			}
		}
		if bad > 0 {
			t.Errorf("%q 共 %d 个模块与参考实现不符", c.text, bad)
		}
	}
}

// 版本选择必须与"实际能装下"一致（含模式与长度指示符的开销）。
func TestQRVersionSelection(t *testing.T) {
	// (字节数, 期望版本)：边界值取自参考实现（字节模式）的实测切换点，
	// 每个版本的"最后一个字节数"都要落在该版本、再加 1 就跳下一版。
	cases := []struct {
		n   int
		ver int
	}{
		{1, 1}, {14, 1},
		{15, 2}, {26, 2},
		{27, 3}, {42, 3},
		{43, 4}, {62, 4},
		{63, 5}, {84, 5},
		{85, 6}, {106, 6},
		{107, 7}, {122, 7},
		{123, 8}, {152, 8},
		{153, 9}, {180, 9},
		{181, 10}, {213, 10},
	}
	for _, c := range cases {
		// 用含小写与符号的内容，确保不会被误判为其它编码模式。
		text := strings.Repeat("a1!x", (c.n+3)/4)[:c.n]
		m, err := QRMatrix(text)
		if err != nil {
			t.Fatalf("%d 字节编码失败：%v", c.n, err)
		}
		if got := (len(m) - 17) / 4; got != c.ver {
			t.Errorf("%d 字节选了 v%d，期望 v%d", c.n, got, c.ver)
		}
	}
}

// 超出容量必须报错，不能生成一张内容被截断的"看起来正常"的码。
func TestQROverflowRejected(t *testing.T) {
	if _, err := QRMatrix(strings.Repeat("a1!", 100)); err == nil {
		t.Fatal("超过版本 10 容量的内容应当报错")
	}
	// 边界：213 字节可编码，214 字节不可（v10/M 字节模式容量 1728 bit）。
	if _, err := QRMatrix(strings.Repeat("a1!", 71)); err != nil { // 213 字节
		t.Errorf("213 字节应当可编码：%v", err)
	}
	if _, err := QRMatrix(strings.Repeat("a1!", 72)); err == nil { // 216 字节
		t.Error("216 字节应当被拒绝")
	}
}

// 分块表必须自洽（init 里已 panic 校验，这里再显式验一遍，
// 使失败信息更直白；同时也是对 qrTotalCodewords 的引用保护）。
func TestQRBlockTableConsistency(t *testing.T) {
	for v := 1; v <= 10; v++ {
		ecc := qrECCM[v]
		data := qrDataCodewords(ecc)
		total := data + ecc[1]*(ecc[2]+ecc[3])
		if total != qrTotalCodewords[v] {
			t.Errorf("v%d：数据 %d + 纠错 %d = %d，规范值 %d",
				v, data, ecc[1]*(ecc[2]+ecc[3]), total, qrTotalCodewords[v])
		}
	}
}

// SVG 必须是自包含的：带命名空间、有静区、白底、且不含外部引用或 data: URI。
func TestQRSVGIsSelfContained(t *testing.T) {
	svg, err := QRSVG("otpauth://totp/x?secret=AAAA&issuer=Server+Probe", 4)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	for _, want := range []string{"<svg", `xmlns="http://www.w3.org/2000/svg"`, "<path", "#ffffff"} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG 缺少 %q：%s", want, svg[:min(120, len(svg))])
		}
	}
	// CSP 是 default-src 'self'：不能引用任何外部资源。
	// 注意 xmlns 的命名空间 URI（http://www.w3.org/2000/svg）是标识符不是请求，
	// 属于合法且必需，故先把它剪掉再检查其它 http(s) 引用。
	body := strings.ReplaceAll(svg, "http://www.w3.org/2000/svg", "")
	for _, bad := range []string{"http://", "https://", "data:", "<script", "<image", "xlink:href", "url("} {
		if strings.Contains(body, bad) {
			t.Errorf("SVG 不应包含 %q（CSP 限制）", bad)
		}
	}
}

func TestQRSVGRejectsOversizedInput(t *testing.T) {
	if _, err := QRSVG(strings.Repeat("a1!", 100), 4); err == nil {
		t.Fatal("超长内容应返回错误而不是半张码")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 由独立参考实现（python-qrcode）生成的期望矩阵，供回归测试使用。
// 每行是一个模块位串按大端压成的整数（第 0 列是最高位）。
var qrGolden = []struct {
	text string
	ver  int
	rows []uint64
}{
	{text: "HELLO WORLD", ver: 1, rows: []uint64{
		2084223, 1071425, 1524061, 1525341, 1530205, 1067329,
		2086271, 1792, 1395218, 1346658, 1146303, 1457170,
		1470964, 7814, 2082359, 1068833, 1531476, 1524342,
		1529173, 1066514, 2087783,
	}},
	{text: "1", ver: 1, rows: []uint64{
		2083967, 1070913, 1527133, 1524317, 1531229, 1066305,
		2086271, 3840, 1395218, 72774, 1002769, 831556,
		24917, 6825, 2084589, 1067960, 1530605, 1524806,
		1529105, 1066054, 2087255,
	}},
	{text: "otpauth://totp/Server%20Probe%3Aops?secret=JBSWY3DPEHPK3PXP&issuer=Server+Probe&algorithm=SHA1&digits=6&period=30", ver: 7, rows: []uint64{
		34910051981695, 18002998125121, 25605745107549, 25577660926813, 25653033697117, 17895435534401,
		35001120150911, 23942370816, 23429986073106, 18715099998217, 32860222984383, 1137918454385,
		27243055493810, 34165352312841, 24640746816137, 19922721133136, 11317773054169, 9950594277945,
		25819857809631, 27665296439065, 35176016494577, 27597498558738, 16843407743831, 3373648135953,
		21942081141241, 32049740845927, 2504844708075, 24441407005152, 20286922423529, 24303510881154,
		30213567621631, 18174533931490, 17577925000256, 23306537276225, 1400723679443, 16543253860544,
		21411350557168, 132759436057, 34963002290517, 17930231329552, 25665751422963, 25588582068633,
		25660171686839, 17891551301634, 35036719453243,
	}},
	{text: "ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!ab!", ver: 7, rows: []uint64{
		34933406910847, 17963886242369, 25610050355805, 25570958248797, 25691487534941, 17929460443201,
		35001120150911, 58201979392, 23397806636050, 34702173405371, 32728809542419, 19369475947192,
		30760943688945, 7323434135563, 18087800804127, 21159459142320, 15845411385585, 14897072245259,
		30623736598557, 6106370521779, 13183938199025, 14374435029275, 5907548279635, 12215744445208,
		26334424145905, 30292435841771, 27836962378063, 13923326343650, 14683105783872, 25539903687179,
		6878813819389, 29936191256803, 20735262457153, 34210922889227, 1411840545267, 16684103219688,
		21195495775729, 97359278363, 34928138981727, 17925400148752, 25640921068017, 25571765717945,
		25693921547039, 17872293968050, 35039569247507,
	}},
}
