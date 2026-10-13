package acme

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"status.example.com": "status.example.com",
		"203.0.113.10":       "203.0.113.10",
		"*.example.com":      "example.com",
		"2001:db8::1":        "2001_db8__1",
		"":                   "cert",
		"...":                "cert",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Fatalf("SanitizeName(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 结果必须是安全的单层文件名：不含分隔符，也不能靠 '.'/'..' 逃出目录。
	for _, in := range []string{"../etc/passwd", "a/b", "..", "/", "\\"} {
		got := SanitizeName(in)
		if got == "" || got == "." || got == ".." {
			t.Fatalf("SanitizeName(%q) 产出了危险文件名 %q", in, got)
		}
		if filepath.Base(got) != got || filepath.IsAbs(got) {
			t.Fatalf("SanitizeName(%q) = %q 不是安全的单层文件名", in, got)
		}
	}
}

// TestAccountKeyIsPersistedAndReused：账户密钥必须落盘且可再次解析出同一把公钥。
// 换了密钥等于换了账户身份，旧账户签发的授权会全部作废。
func TestAccountKeyIsPersistedAndReused(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateAccountKey(dir)
	if err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, accountKeyFile)); err != nil {
		t.Fatalf("账户密钥未落盘: %v", err)
	}
	second, err := loadOrCreateAccountKey(dir)
	if err != nil {
		t.Fatalf("再次加载失败: %v", err)
	}
	if !first.PublicKey.Equal(&second.PublicKey) {
		t.Fatal("重新加载出的账户密钥与首次不一致（落盘/解析有误）")
	}

	// 私钥文件不应是"谁都能读"的权限（Windows 上权限位无意义，跳过）。
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, accountKeyFile))
		if err != nil {
			t.Fatalf("stat 失败: %v", err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("账户私钥权限应为 0600，实际 %o", perm)
		}
	}
}

func TestAccountKeyMemoryOnlyWhenNoCacheDir(t *testing.T) {
	key, err := loadOrCreateAccountKey("")
	if err != nil {
		t.Fatalf("无缓存目录时应能在内存里生成: %v", err)
	}
	if key == nil {
		t.Fatal("应返回一把可用的密钥")
	}
}

func TestAccountKeyRejectsCorruptPEM(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, accountKeyFile), []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("写坏文件失败: %v", err)
	}
	if _, err := loadOrCreateAccountKey(dir); err == nil {
		t.Fatal("损坏的账户密钥应报错，而不是静默换一把新的（那会让账户身份漂移）")
	}
}

func TestAccountKeyRejectsNonECDSA(t *testing.T) {
	dir := t.TempDir()
	// 把 RSA 密钥塞进 account.key：类型不符必须报错。
	der := rsaKeyPEM(t)
	if err := os.WriteFile(filepath.Join(dir, accountKeyFile), der, 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := loadOrCreateAccountKey(dir); err == nil {
		t.Fatal("非 ECDSA 的账户密钥应报错")
	}
}

func TestAccountURLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadAccountURL(dir); err == nil {
		t.Fatal("文件不存在时应报错")
	}
	const kid = "https://acme.example.com/acct/42"
	if err := saveAccountURL(dir, kid); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := loadAccountURL(dir)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got != kid {
		t.Fatalf("往返不一致: %q", got)
	}
	// 空 URL 表示"kid 失效"，应当删掉文件而不是写一个空值进去
	// （写空值会让下次启动读到一个非法 URL）。
	if err := saveAccountURL(dir, ""); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, accountURLFile)); !os.IsNotExist(err) {
		t.Fatal("清空后账户 URL 文件应被删除")
	}
}

func TestLoadAccountURLRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, accountURLFile), []byte("不是 URL\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := loadAccountURL(dir); err == nil {
		t.Fatal("非 URL 内容应被拒绝")
	}
}

// rsaKeyPEM 返回一段 PKCS#8 编码的 RSA 私钥 PEM（用于「类型不符」用例）。
// 用 1024 位即可满足用途，生成快。
func rsaKeyPEM(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("生成 RSA 密钥失败: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatalf("编码 RSA 私钥失败: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
