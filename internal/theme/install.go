package theme

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// 主题包安装（后台上传 zip）。
//
// 约定：**zip 文件名去掉 .zip 就是主题名**，解压到 themesDir/<主题名>/。
// 同名视为"同一个主题的新版本"——整目录替换，而不是合并：合并会把上一版
// 多出来的文件留在新主题里（旧的 app.js 与新的 index.html 混在一起，
// 症状是"改了却没生效"，极难排查）。

const (
	// MaxZipBytes 是单包上限。主题是纯静态资源，32 MB 已经非常宽松；
	// 上限的作用是挡住误传的大文件与解压炸弹。
	MaxZipBytes = 32 << 20
	// 单个文件上限（防 zip 炸弹：压缩比极高的小包解出超大文件）。
	maxThemeFileBytes = 16 << 20
	// 文件条目上限（正常情况下一个主题几十个文件）。
	maxThemeZipEntries = 800
)

// InstallZip 把 zipPath 安装为名为 name 的主题，同名则整体替换。
//
// 返回最终的主题名（已规范化）。
func (m *Manager) InstallZip(zipPath, name string) (string, error) {
	name, err := sanitizeThemeName(name)
	if err != nil {
		return "", err
	}
	if name == "builtin" {
		// builtin 是内嵌主题，同名会让"回退到内置"变得不可能。
		return "", fmt.Errorf("builtin 是内置主题的保留名，请换一个主题包名")
	}
	if m.dir == "" {
		return "", fmt.Errorf("未配置 themes_dir，无法安装外部主题")
	}
	if fi, err := os.Stat(zipPath); err != nil {
		return "", fmt.Errorf("读取主题包失败: %w", err)
	} else if fi.Size() > MaxZipBytes {
		return "", fmt.Errorf("主题包过大（上限 %d MB）", MaxZipBytes>>20)
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return "", fmt.Errorf("创建主题目录失败: %w", err)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("不是有效的 zip：%w", err)
	}
	defer zr.Close()
	if len(zr.File) == 0 {
		return "", fmt.Errorf("主题包是空的")
	}
	if len(zr.File) > maxThemeZipEntries {
		return "", fmt.Errorf("主题包文件数过多（上限 %d）", maxThemeZipEntries)
	}

	// 先解到临时目录：中途失败（坏包/超限/缺 theme.json）时线上主题完全不受影响。
	tmp, err := os.MkdirTemp(m.dir, ".install-*")
	if err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	// 成功路径下 tmp 会被 rename 走，所以这里只负责失败清理。
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()

	prefix := commonTopDir(zr.File)
	for _, f := range zr.File {
		rel := strings.TrimPrefix(f.Name, prefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" || strings.HasSuffix(f.Name, "/") {
			continue
		}
		if !safeThemeRel(rel) {
			return "", fmt.Errorf("主题包含非法路径：%s", f.Name)
		}
		dst := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", fmt.Errorf("创建目录失败: %w", err)
		}
		if err := extractOne(f, dst); err != nil {
			return "", err
		}
	}

	// 校验：没有 theme.json 就不是主题（scanExternal 也会直接忽略它，
	// 装进去只会得到"上传成功但列表里没有"）。
	if _, err := os.Stat(filepath.Join(tmp, "theme.json")); err != nil {
		return "", fmt.Errorf("主题包缺少 theme.json（zip 根目录或唯一顶层目录下需要有 theme.json 与 index.html）")
	}

	dst := filepath.Join(m.dir, name)
	backup := ""
	if _, err := os.Stat(dst); err == nil {
		backup = dst + ".old-" + time.Now().Format("20060102-150405")
		if err := os.Rename(dst, backup); err != nil {
			return "", fmt.Errorf("备份旧主题失败: %w", err)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		// 换不上去就把旧主题放回来——否则这个主题名就永久消失了。
		if backup != "" {
			_ = os.Rename(backup, dst)
		}
		return "", fmt.Errorf("写入主题目录失败: %w", err)
	}
	ok = true
	if backup != "" {
		_ = os.RemoveAll(backup)
	}

	if err := m.reload(); err != nil {
		return "", err
	}
	return name, nil
}

// reload 重新扫描外部主题（内置主题保留）。
func (m *Manager) reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 保留 builtin，清掉外部条目后重扫。
	for k, v := range m.themes {
		if !v.info.Builtin {
			delete(m.themes, k)
		}
	}
	return m.scanExternal()
}

// extractOne 解出单个文件，并限制解压后大小。
func extractOne(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", f.Name, err)
	}
	defer rc.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("写入 %s 失败: %w", f.Name, err)
	}
	defer out.Close()

	// +1 是为了"恰好等于上限"时也能读出来判断是否超限。
	n, err := io.Copy(out, io.LimitReader(rc, maxThemeFileBytes+1))
	if err != nil {
		return fmt.Errorf("解压 %s 失败: %w", f.Name, err)
	}
	if n > maxThemeFileBytes {
		return fmt.Errorf("主题包内单文件过大（%s 超过 %d MB）", f.Name, maxThemeFileBytes>>20)
	}
	return nil
}

// sanitizeThemeName 校验并规范化主题名。
//
// 主题名要做目录名，因此必须挡住：路径分隔符、Windows 保留字符、
// 纯点（. / ..）、以点开头（会被 scanExternal 当隐藏目录跳过）。
// 允许中文——用户就是拿"毛玻璃.zip"这种名字上传的。
func sanitizeThemeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".zip")
	name = strings.TrimSuffix(name, ".ZIP")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("主题名不能为空（主题名取自 zip 文件名）")
	}
	if utf8.RuneCountInString(name) > 64 {
		return "", fmt.Errorf("主题名过长（上限 64 个字符）")
	}
	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("主题名不能以点开头")
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) {
		return "", fmt.Errorf(`主题名不能包含 / \ : * ? " < > |`)
	}
	for _, r := range name {
		if r < 0x20 {
			return "", fmt.Errorf("主题名不能包含控制字符")
		}
	}
	return name, nil
}

// safeThemeRel 校验解压用的相对路径（zip-slip）。
func safeThemeRel(rel string) bool {
	rel = strings.ReplaceAll(rel, "\\", "/")
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, ":") {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// commonTopDir 返回所有条目共享的唯一顶层目录（含结尾的 "/"），没有则返回 ""。
//
// 为什么需要它：用户在资源管理器里"右键 → 压缩文件夹"，zip 里会多一层
// 与文件夹同名的目录。不剥掉的话，主题会被解成
// themesDir/<name>/<name>/index.html，而加载器只认 themesDir/<name>/theme.json
// ——表现是"上传成功、列表里却没有"。
func commonTopDir(files []*zip.File) string {
	top := ""
	for _, f := range files {
		n := strings.TrimPrefix(strings.ReplaceAll(f.Name, "\\", "/"), "/")
		if n == "" {
			continue
		}
		i := strings.Index(n, "/")
		if i < 0 {
			// 根目录直接有文件 → 没有统一顶层目录。
			return ""
		}
		d := n[:i+1]
		if top == "" {
			top = d
		} else if top != d {
			return ""
		}
	}
	// 只有"唯一的顶层目录"才剥；同时确认剥完之后 theme.json 会落在根上。
	if top == "" {
		return ""
	}
	for _, f := range files {
		n := strings.TrimPrefix(strings.ReplaceAll(f.Name, "\\", "/"), "/")
		if strings.TrimPrefix(n, top) == "theme.json" {
			return top
		}
	}
	return ""
}
