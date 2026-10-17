package theme

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeZip 在临时目录里造一个 zip，entries 为 路径→内容。
func makeZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pkg.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 zip 失败: %v", err)
	}
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("写条目 %s 失败: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("写内容 %s 失败: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	_ = f.Close()
	return p
}

func metaJSON() string {
	return `{"name":"Demo","version":"1.0.0","api_version":"v1","entry":"index.html"}`
}

// 主题包的两种常见形态都要能装：根目录直接放文件；或多一层同名文件夹。
func TestInstallZipAcceptsFlatAndNested(t *testing.T) {
	flat := map[string]string{"theme.json": metaJSON(), "index.html": "<html></html>"}
	nested := map[string]string{"demo/theme.json": metaJSON(), "demo/index.html": "<html></html>"}
	for name, entries := range map[string]map[string]string{"flat": flat, "nested": nested} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			m, err := NewManager(dir)
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			got, err := m.InstallZip(makeZip(t, entries), "demo")
			if err != nil {
				t.Fatalf("安装失败: %v", err)
			}
			if got != "demo" {
				t.Errorf("主题名应为 demo，得到 %q", got)
			}
			// 关键：多一层目录时也要落在 <dir>/demo/theme.json，
			// 否则加载器扫不到（症状是"上传成功但列表里没有"）。
			if _, err := os.Stat(filepath.Join(dir, "demo", "theme.json")); err != nil {
				t.Errorf("theme.json 未落在主题根目录: %v", err)
			}
			if !m.Exists("demo") {
				t.Error("安装后应能在主题列表里看到 demo")
			}
		})
	}
}

// 同名安装＝更新：整目录替换，旧的多余文件不能留着。
func TestInstallZipReplacesSameName(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := m.InstallZip(makeZip(t, map[string]string{
		"theme.json": metaJSON(), "index.html": "v1", "old.js": "old",
	}), "demo"); err != nil {
		t.Fatalf("首次安装失败: %v", err)
	}
	if _, err := m.InstallZip(makeZip(t, map[string]string{
		"theme.json": metaJSON(), "index.html": "v2",
	}), "demo"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	body, err := m.Open("demo", "index.html")
	if err != nil || string(body) != "v2" {
		t.Fatalf("应为新版本内容 v2，得到 %q (%v)", string(body), err)
	}
	// 整目录替换：上一版独有的 old.js 必须消失，否则新旧文件混在一起。
	if _, err := os.Stat(filepath.Join(dir, "demo", "old.js")); err == nil {
		t.Error("同名更新应整目录替换，旧文件 old.js 不该留下")
	}
}

func TestInstallZipRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	t.Run("缺少theme_json", func(t *testing.T) {
		if _, err := m.InstallZip(makeZip(t, map[string]string{"index.html": "x"}), "demo"); err == nil {
			t.Fatal("没有 theme.json 应拒绝")
		}
	})

	t.Run("zip_slip", func(t *testing.T) {
		z := makeZip(t, map[string]string{
			"theme.json": metaJSON(), "../evil.txt": "pwn",
		})
		if _, err := m.InstallZip(z, "demo"); err == nil {
			t.Fatal("含 ../ 的条目应拒绝")
		}
	})

	t.Run("内置主题名保留", func(t *testing.T) {
		z := makeZip(t, map[string]string{"theme.json": metaJSON()})
		if _, err := m.InstallZip(z, "builtin"); err == nil {
			t.Fatal("builtin 是保留名，应拒绝")
		}
	})

	t.Run("非法主题名", func(t *testing.T) {
		z := makeZip(t, map[string]string{"theme.json": metaJSON()})
		for _, bad := range []string{"", "..", ".hidden", "a/b", `a\b`, "a:b"} {
			if _, err := m.InstallZip(z, bad); err == nil {
				t.Errorf("主题名 %q 应被拒绝", bad)
			}
		}
	})
}

// 中文名（"毛玻璃.zip"）是常见用法，不能被误拒。
func TestSanitizeThemeNameAllowsChinese(t *testing.T) {
	got, err := sanitizeThemeName("毛玻璃.zip")
	if err != nil {
		t.Fatalf("中文主题名应允许: %v", err)
	}
	if got != "毛玻璃" {
		t.Errorf("应去掉 .zip 后缀，得到 %q", got)
	}
	if strings.Contains(got, ".zip") {
		t.Error("主题名不应包含扩展名")
	}
}
