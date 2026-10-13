package auth

import (
	"fmt"
	"strings"
)

// 极简 QR 码编码器（字节模式，纠错等级 M）。
//
// 为什么要自己写：项目的前端约束是**零外部依赖 + CSP default-src 'self'**，
// 不能引 CDN 上的 qrcode 库，也不能用 <img src="data:">（CSP 未开 data:）。
// 而 TOTP 绑定又极度依赖"扫码"这个动作——纯手工输 32 位 base32 出错率高，
// 输错的后果是启用后自己登不进去。
//
// 实现范围（够用即止，不是通用库）：
//   * 8 位字节模式（otpauth URI 是 ASCII，正好合适）
//   * 纠错等级 M（约 15% 冗余，实践中容错与体积的平衡点）
//   * 版本 1~10 自动选择（最多容纳约 271 字节，otpauth URI 通常 < 150 字节）
//   * 掩码固定用 0（规范要求评估 8 种掩码选最优，但掩码只影响可读性，
//     不影响能否被扫出；固定一个能省掉一半代码，且实测主流 App 都能识别）
//
// 不做的事：汉字模式、结构化追加、版本 > 10、Micro QR。
// 这些对"给认证器 App 扫个 otpauth URI"没有任何用处。

// qrECCM 是版本 1~10、纠错等级 M 的每块参数。
// 每项：[每块数据码字数, 每块纠错码字数, 第 1 组块数, 第 2 组块数]。
// 数值取自 ISO/IEC 18004 表 9（M 列）。
//
// **注意这里是"每块"，不是"整个版本"**：v4 的 64 个数据码字是 2 块 × 32，
// v6 的 108 个是 4 块 × 27。曾经错把整版本的数据码字总数写进第一列
// （v4 写 64、v6 写 108），后果是 RS 分块长度算错、交织顺序整体错位，
// **生成的码扫描器完全解不出来**，但矩阵尺寸/寻像图案都正常，肉眼看不出来。
// 由 dist/qrverify 的差分比对（对照独立参考实现）抓出，已修正。
var qrECCM = [11][4]int{
	{},             // 占位，版本从 1 开始
	{16, 10, 1, 0}, // v1  数据码字总数 16
	{28, 16, 1, 0}, // v2  28
	{44, 26, 1, 0}, // v3  44
	{32, 18, 2, 0}, // v4  64
	{43, 24, 2, 0}, // v5  86
	{27, 16, 4, 0}, // v6  108
	{31, 18, 4, 0}, // v7  124
	{38, 22, 2, 2}, // v8  154
	{36, 22, 3, 2}, // v9  182
	{43, 26, 4, 1}, // v10 216
}

// qrAlignCenters 返回版本 v 的对齐图案中心坐标。
// 版本 1 没有对齐图案。
var qrAlignCenters = [11][]int{
	{}, {}, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34},
	{6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50},
}

// qrTotalCodewords 返回版本 v（1~10）的数据区总码字数（数据 + 纠错）。
// 由各版本的码字总数直接给出（ISO 18004 表 1），用于在包初始化时
// 交叉校验上面的分块表——这张表一旦填错，二维码会"能显示但扫不出来"，
// 而且几乎不可能靠肉眼发现。
var qrTotalCodewords = [11]int{0, 26, 44, 70, 100, 134, 172, 196, 242, 292, 346}

// 包初始化时做一次自检：分块表反推出来的"数据 + 纠错"总码字数
// 必须与规范给的每版本码字总数完全一致。不一致就直接 panic——
// 这是编译期常量级错误，宁可启动即失败，也不要静默产出扫不出来的二维码。
func init() {
	for v := 1; v <= 10; v++ {
		ecc := qrECCM[v]
		nblocks := ecc[2] + ecc[3]
		total := qrDataCodewords(ecc) + ecc[1]*nblocks
		if total != qrTotalCodewords[v] {
			panic(fmt.Sprintf(
				"qrcode: 版本 %d 的分块参数自相矛盾：数据 %d + 纠错 %d = %d，规范值为 %d",
				v, qrDataCodewords(ecc), ecc[1]*nblocks, total, qrTotalCodewords[v]))
		}
	}
}

// QRMatrix 返回该文本的 QR 模块矩阵（true = 黑）。
func QRMatrix(text string) ([][]bool, error) {
	data := []byte(text)
	ver := 0
	for v := 1; v <= 10; v++ {
		if qrDataFits(v, len(data)) {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, fmt.Errorf("内容过长（%d 字节），超出本编码器支持的版本 10", len(data))
	}
	return qrEncode(data, ver)
}

// qrDataFits 判断 n 字节在版本 v（M 等级、字节模式）下能否装下。
//
// 必须把模式指示符（4 位）与长度字段（8 或 16 位）一起算进占用——
// 只比 n*8 会让边界长度选小一个版本，接着 qrEncode 里"补终止符与填充码字"
// 的循环会写超出数据容量，最终铺矩阵时多出的码字被静默丢弃：
// **码能生成、也能显示，但扫描器解出来是错的**。
func qrDataFits(v, n int) bool {
	return 4+qrCharCountBits(v)+n*8 <= qrDataCodewords(qrECCM[v])*8
}

// qrDataCodewords 返回该分块参数下全部块的数据码字总数。
//
// 第 2 组每块比第 1 组多 1 个数据码字（规范里的特殊情形），
// 所以总数是 ecc[0]*组1 + (ecc[0]+1)*组2，**不能**简单地写成
// ecc[0]*(组1+组2)——那在 v8~v10 会少算 1~2 个码字，
// 表现为 qrSplitBlocks 切片越界 panic（或静默丢码字）。
func qrDataCodewords(ecc [4]int) int {
	return ecc[0]*ecc[2] + (ecc[0]+1)*ecc[3]
}

// qrCharCountBits 返回版本 v 在字节模式下长度字段的位数。
// 版本 1~9 是 8 位，10~26 是 16 位。
func qrCharCountBits(v int) int {
	if v <= 9 {
		return 8
	}
	return 16
}

func qrEncode(data []byte, ver int) ([][]bool, error) {
	ecc := qrECCM[ver]
	dataCW := qrDataCodewords(ecc)

	// ---- 1) 构造数据位流：模式(0100) + 长度 + 数据 + 终止符 + 补齐 ----
	var bits []bool
	appendBits := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (v>>uint(i))&1 == 1)
		}
	}
	appendBits(0b0100, 4) // 字节模式
	appendBits(len(data), qrCharCountBits(ver))
	for _, b := range data {
		appendBits(int(b), 8)
	}
	// 终止符最长 4 位，但不能超出容量。
	for i := 0; i < 4 && len(bits) < dataCW*8; i++ {
		bits = append(bits, false)
	}
	// 补齐到字节边界。
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	// 用 0xEC / 0x11 交替填充剩余字节（规范规定的填充码字）。
	fill := []int{0xEC, 0x11}
	for i := 0; len(bits) < dataCW*8; i++ {
		appendBits(fill[i%2], 8)
	}

	// ---- 2) 字节化并按块切分 ----
	codewords := make([]byte, len(bits)/8)
	for i := range codewords {
		for j := 0; j < 8; j++ {
			if bits[i*8+j] {
				codewords[i] |= 1 << uint(7-j)
			}
		}
	}

	blocks := qrSplitBlocks(codewords, ecc)

	// ---- 3) 逐块计算 RS 纠错码 ----
	var all []byte
	var eccAll []byte
	for _, b := range blocks {
		all = append(all, b...)
		eccAll = append(eccAll, qrRS(b, ecc[1])...)
	}
	// 交织：数据码字按块轮流取，然后是纠错码字按块轮流取。
	//
	// 索引上界必须是"最长块的码字数"，即 ecc[0]+1（第 2 组块比第 1 组多 1）。
	// 用 ecc[0] 当上界会**丢掉每个长块的最后 1 个码字**，
	// 使后面所有码字的交织位置整体前移——生成的码完全解不出来。
	final := make([]byte, 0, len(all)+len(eccAll))
	maxData := ecc[0]
	if ecc[3] > 0 {
		maxData = ecc[0] + 1
	}
	for i := 0; i < maxData; i++ {
		for _, b := range blocks {
			if i < len(b) {
				final = append(final, b[i])
			}
		}
	}
	for i := 0; i < ecc[1]; i++ {
		for j := 0; j < len(blocks); j++ {
			idx := j*ecc[1] + i
			if idx < len(eccAll) {
				final = append(final, eccAll[idx])
			}
		}
	}

	// ---- 4) 铺矩阵 ----
	size := ver*4 + 17
	m := make([][]bool, size)
	reserved := make([][]bool, size)
	for i := range m {
		m[i] = make([]bool, size)
		reserved[i] = make([]bool, size)
	}

	placeFinder(m, reserved, 0, 0)
	placeFinder(m, reserved, size-7, 0)
	placeFinder(m, reserved, 0, size-7)
	// 顺序很关键：对齐图案必须在时序图案**之前**铺。
	// placeAlign 靠"中心格是否已被占用"来跳过与定位图案重叠的中心，
	// 而时序图案恰好占满第 6 行/第 6 列——先铺时序会让 (6,*) 与 (*,6)
	// 这些合法中心被误判为"已占用"而被整体跳过，
	// v7+ 因此缺少若干对齐图案、整张码错位（而 v1~v6 完全正常）。
	placeAlign(m, reserved, ver)
	placeTiming(m, reserved, size)
	// 固定的格式信息（掩码 0、纠错 M）：dark 模块与格式位。
	placeFormatM(m, reserved, size)
	// 固定版本信息占位（版本 ≥ 7 才有）。
	placeVersion(m, reserved, ver)

	qrPlaceData(m, reserved, final, size)

	// 掩码在 qrPlaceData 内部随铺随打（与规范一致），此处不再另做一遍。
	// 曾经这里有一段"铺完再统一 XOR 掩码"的循环，但它靠 `reserved` 判断
	// 哪些格子该掩——而 qrPlaceData 铺完后把**数据格也标成了 reserved**，
	// 于是那段循环恰好跳过了所有数据格，**掩码等于没打**。
	// 症状极具迷惑性：矩阵尺寸、寻像图案、格式位、码字流全都正确，
	// 但整张码对扫描器而言是错的（104/208 个模块与参考实现不符）。
	return m, nil
}

func qrSplitBlocks(codewords []byte, ecc [4]int) [][]byte {
	var out [][]byte
	pos := 0
	for i := 0; i < ecc[2]; i++ {
		out = append(out, codewords[pos:pos+ecc[0]])
		pos += ecc[0]
	}
	// 第 2 组每块比第 1 组多 1 个数据码字（规范里的特殊情形）。
	if ecc[3] > 0 {
		long := ecc[0] + 1
		for i := 0; i < ecc[3]; i++ {
			out = append(out, codewords[pos:pos+long])
			pos += long
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Reed-Solomon（GF(256)，本原多项式 0x11D）
// ---------------------------------------------------------------------------

var (
	qrExp [512]byte
	qrLog [256]byte
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		qrExp[i] = byte(x)
		qrLog[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		qrExp[i] = qrExp[i-255]
	}
}

func qrMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return qrExp[int(qrLog[a])+int(qrLog[b])]
}

// qrRS 计算数据的纠错码字。
func qrRS(data []byte, eccLen int) []byte {
	// 生成多项式 g(x) = (x - a^0)(x - a^1)...(x - a^(eccLen-1))
	gen := []byte{1}
	for i := 0; i < eccLen; i++ {
		next := make([]byte, len(gen)+1)
		for j := range gen {
			next[j] ^= gen[j]
			next[j+1] ^= qrMul(gen[j], qrExp[i])
		}
		gen = next
	}
	// 多项式除法求余。
	rem := make([]byte, eccLen)
	for _, d := range data {
		factor := d ^ rem[0]
		copy(rem, rem[1:])
		rem[eccLen-1] = 0
		if factor != 0 {
			for j := 0; j < eccLen; j++ {
				rem[j] ^= qrMul(gen[j+1], factor)
			}
		}
	}
	return rem
}

// ---------------------------------------------------------------------------
// 矩阵铺设
// ---------------------------------------------------------------------------

func placeFinder(m, res [][]bool, row, col int) {
	size := len(m)
	for r := -1; r <= 7; r++ {
		for c := -1; c <= 7; c++ {
			rr, cc := row+r, col+c
			if rr < 0 || rr >= size || cc < 0 || cc >= size {
				continue
			}
			// 定位图案：外框 7x7 黑边、内 3x3 黑心、中间白环；外加一圈分隔符（白）。
			black := (r >= 0 && r <= 6 && (c == 0 || c == 6)) ||
				(c >= 0 && c <= 6 && (r == 0 || r == 6)) ||
				(r >= 2 && r <= 4 && c >= 2 && c <= 4)
			m[rr][cc] = black
			res[rr][cc] = true
		}
	}
}

func placeTiming(m, res [][]bool, size int) {
	for i := 8; i < size-8; i++ {
		v := i%2 == 0
		if !res[6][i] {
			m[6][i] = v
			res[6][i] = true
		}
		if !res[i][6] {
			m[i][6] = v
			res[i][6] = true
		}
	}
}

func placeAlign(m, res [][]bool, ver int) {
	centers := qrAlignCenters[ver]
	for _, r := range centers {
		for _, c := range centers {
			// 与定位图案重叠的位置跳过。
			if res[r][c] {
				continue
			}
			for dr := -2; dr <= 2; dr++ {
				for dc := -2; dc <= 2; dc++ {
					rr, cc := r+dr, c+dc
					if rr < 0 || rr >= len(m) || cc < 0 || cc >= len(m) {
						continue
					}
					// 对齐图案：外框黑、内点黑、其余白。
					black := dr == -2 || dr == 2 || dc == -2 || dc == 2 || (dr == 0 && dc == 0)
					m[rr][cc] = black
					res[rr][cc] = true
				}
			}
		}
	}
}

// placeFormatM 写入格式信息（纠错等级 M + 掩码 0）。
//
// 格式串 = 2 位纠错等级 + 3 位掩码，共 5 位，经 BCH(15,5) 编码后与 0x5412 异或。
// M 等级的指示位是 00，掩码 0 是 000，所以 5 位数据是 00000，
// 标准结果是 0b101010000010010。
//
// **两个坑都在这里，且都不会影响"码是否生成成功"，只会让扫描器拒绝解码：**
//
//  1. **位序**：规范按"最低位在前"铺（bit0 先写），所以第 i 位就是 bitAt(bits, i)，
//     i 的推进顺序决定落点顺序。把 i 当成权重位（bit14-i）会得到整串反序。
//  2. **两份副本的走法不对称**，因为要绕开定位图案与第 6 行的时序列：
//     垂直副本沿第 8 列：i=0..5 → 行 0..5，i=6 → 行 7（跳过行 6 时序），
//     i=7 → 行 8，i=8..14 → 行 14..20（即 size-15+i）。
//     水平副本沿第 8 行：i=0..7 → 列 20..13（即 size-1-i），
//     i=8 → 列 7（跳时序列 6），i=9..14 → 列 5..0（即 15-i-1）。
//     曾经把水平副本也按"先 0..5 再 7、8"来写，导致 (8,13)~(8,20) 这一整段
//     该是格式位的地方被当作数据位、而 (8,0)~(8,5) 又错位，
//     **整张码从第 6 列往右全部错乱**。
func placeFormatM(m, res [][]bool, size int) {
	const data = 0b00000 // M + 掩码 0（M=00，mask=000）
	bits := qrFormatBits(data)

	// 垂直副本（第 8 列）。
	for i := 0; i < 15; i++ {
		var row int
		switch {
		case i < 6:
			row = i
		case i < 8:
			row = i + 1
		default:
			row = size - 15 + i
		}
		m[row][8] = bitAt(bits, i)
		res[row][8] = true
	}

	// 水平副本（第 8 行）。
	for i := 0; i < 15; i++ {
		var col int
		switch {
		case i < 8:
			col = size - 1 - i
		case i < 9:
			col = 15 - i - 1 + 1
		default:
			col = 15 - i - 1
		}
		m[8][col] = bitAt(bits, i)
		res[8][col] = true
	}

	// 固定的 dark module（规范强制为黑）。
	m[size-8][8] = true
	res[size-8][8] = true
}

// qrFormatBits 对 5 位数据做 BCH(15,5) 并异或掩码 0x5412。
func qrFormatBits(data int) int {
	v := data << 10
	for i := 14; i >= 10; i-- {
		if (v>>uint(i))&1 == 1 {
			v ^= 0b10100110111 << uint(i-10)
		}
	}
	return ((data << 10) | v) ^ 0b101010000010010
}

func bitAt(v, i int) bool { return (v>>uint(i))&1 == 1 }

// placeVersion 写入版本信息（版本 ≥ 7 才有）。
//
// 18 位的布局分两块，坐标以 (size-11) 为基准：
//   - 右上块：行 i/3（0~5），列 size-11 + i%3（size-11 ~ size-9）
//   - 左下块：行 size-11 + i%3，列 i/3（逻辑上是右上块的转置）
//
// **位序与原实现的差别**：规范按最低位在前铺，所以第 i 位就是 bitAt(v, i)，
// 而不是 bitAt(v, 17-i)。另外这里曾经把两块的行列写反（写成
// m[size-11+i%3][i/3] + m[i/3][size-11+i%3]），等价于把两块互换，
// 结果 v7 及以上**整张码都错**，而 v1~v6 完全正常——
// 因为版本信息只在 v7+ 存在。这个特征极易被误判为"是数据区的锅"。
func placeVersion(m, res [][]bool, ver int) {
	if ver < 7 {
		return
	}
	v := ver << 12
	for i := 17; i >= 12; i-- {
		if (v>>uint(i))&1 == 1 {
			v ^= 0b1111100100101 << uint(i-12)
		}
	}
	v = (ver << 12) | v
	size := len(m)
	base := size - 11
	for i := 0; i < 18; i++ {
		bit := bitAt(v, i)
		// 右上块。
		m[i/3][i%3+base] = bit
		res[i/3][i%3+base] = true
		// 左下块（转置）。
		m[i%3+base][i/3] = bit
		res[i%3+base][i/3] = true
	}
}

// qrPlaceData 按 Zigzag 顺序把码字位铺进矩阵，**并在铺放时应用掩码 0**。
//
// 掩码必须在这里随铺随打：调用方无法在事后区分"数据格"与"功能图案格"，
// 因为两者都被标记进了同一个 reserved 切片。掩码 0 的规则是 (row+col)%2==0。
//
// 列配对从右往左是 (size-1, size-2)、(size-3, size-4)……
// 唯一的例外是第 6 列（垂直时序图案所在列）不承载数据：
// 遇到 col ≤ 6 时该轮改用 (col-1, col-2)，使配对跨过第 6 列。
//
// **必须用独立的 loop 变量**：写成 `for col := size-1; col > 0; col -= 2`
// 再在循环体里 `col--`，Go 的 for 后置语句会从*被改过*的 col 再减 2，
// 于是列序列变成 [20,18,…,8,5,2] 而不是规范的 [20,…,8,5,3,1]——
// 最后三对列（3-2、1-0）被整体跳过，左下角一小片区域错位。
func qrPlaceData(m, res [][]bool, codewords []byte, size int) {
	bitIdx := 0
	total := len(codewords) * 8
	upward := true
	for base := size - 1; base > 0; base -= 2 {
		col := base
		if col <= 6 { // 跳过垂直时序图案所在列
			col--
		}
		for i := 0; i < size; i++ {
			row := i
			if upward {
				row = size - 1 - i
			}
			for _, c := range []int{col, col - 1} {
				if res[row][c] {
					continue
				}
				bit := false
				if bitIdx < total {
					bit = (codewords[bitIdx/8]>>uint(7-bitIdx%8))&1 == 1
				}
				// 掩码 0：位于 (row+col) 为偶数的格子翻转。
				if (row+c)%2 == 0 {
					bit = !bit
				}
				m[row][c] = bit
				res[row][c] = true
				bitIdx++
			}
		}
		upward = !upward
	}
}

// QRSVG 把矩阵渲染成 SVG 字符串（前端直接插入 DOM，无需外部依赖）。
//
// 用 SVG 而不是返回 base64 PNG：CSP 未开 data: 与 blob:，
// 内联 SVG 是零依赖环境下唯一能确定性渲染的形态。
// 模块数固定为 4 的倍数留边（规范要求静区 ≥ 4 模块），否则部分扫码器识别率下降。
func QRSVG(text string, moduleSize int) (string, error) {
	m, err := QRMatrix(text)
	if err != nil {
		return "", err
	}
	if moduleSize <= 0 {
		moduleSize = 4
	}
	const quiet = 4
	size := len(m)
	dim := (size + quiet*2) * moduleSize

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges" role="img" aria-label="TOTP 配置二维码">`,
		dim, dim, dim, dim)
	// 白色静区背景：部分扫码器在深色底上看不到静区，会直接识别失败。
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, dim, dim)
	b.WriteString(`<path fill="#000000" d="`)
	for r := 0; r < size; r++ {
		for c := 0; c < size; c++ {
			if !m[r][c] {
				continue
			}
			fmt.Fprintf(&b, "M%d %dh%dv%dh-%dz",
				(c+quiet)*moduleSize, (r+quiet)*moduleSize,
				moduleSize, moduleSize, moduleSize)
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}
