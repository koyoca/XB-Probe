package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yourorg/probe/internal/config"
	"github.com/yourorg/probe/internal/tlsconf"
)

// writeTLSConfig 在临时目录里造一份能加载的 master.yaml，并返回它的路径。
func writeTLSConfig(t *testing.T, dir, tlsBlock string) string {
	t.Helper()
	// 路径用 YAML 的**单引号**包裹：双引号串里 `\U`、`\A` 都是转义序列，
	// Windows 的 `C:\Users\...` 会让解析器直接报"缺少十六进制数字"。
	path := filepath.Join(dir, "master.yaml")
	body := "data_dir: '" + dir + "'\n" +
		"master_key_file: '" + filepath.Join(dir, "master.key") + "'\n" +
		"public_listen: \"127.0.0.1:18080\"\n" +
		tlsBlock
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	return path
}

// loadCfgForTLS 用真配置加载器读一遍——状态文件的路径必须由
// tlsconf.StatusPath 从**真实加载后的**配置推导，而不是在测试里另写一遍。
func loadCfgForTLS(t *testing.T, path string) *config.MasterConfig {
	t.Helper()
	cfg, err := config.LoadMaster(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return cfg
}

func writeStatus(t *testing.T, path string, st tlsconf.Status) {
	t.Helper()
	if st.UpdatedAt == 0 {
		st.UpdatedAt = time.Now().Unix()
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("序列化状态失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写状态失败: %v", err)
	}
}

const acmeTLSBlock = `tls:
  mode: acme
  acme:
    domains: ["status.example.com"]
    challenge: http-01
`

// TestTLSStatusReadyIsSuccess：有证书时必须报告成功并返回 0。
func TestTLSStatusReadyIsSuccess(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTLSConfig(t, dir, acmeTLSBlock)
	loaded := loadCfgForTLS(t, cfg)
	now := time.Now()
	writeStatus(t, tlsconf.StatusPath(loaded.TLS, loaded.DataDir), tlsconf.Status{
		Mode: "acme", Source: "acme", Challenge: "http-01",
		Domains: []string{"status.example.com"},
		Subject: "status.example.com", Issuer: "Stub CA",
		NotBefore: now.Add(-time.Hour).Unix(), NotAfter: now.AddDate(0, 3, 0).Unix(),
		SPKISHA256: "abc",
	})
	if err := runTLS([]string{"status", "-config", cfg}); err != nil {
		t.Fatalf("证书已就绪时不应报错：%v", err)
	}
}

// TestTLSStatusFreshFailureIsFailure：本轮失败的申请必须报成失败（退出码 1 的那条路径）。
func TestTLSStatusFreshFailureIsFailure(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTLSConfig(t, dir, acmeTLSBlock)
	loaded := loadCfgForTLS(t, cfg)
	writeStatus(t, tlsconf.StatusPath(loaded.TLS, loaded.DataDir), tlsconf.Status{
		Mode: "acme", Challenge: "http-01", Domains: []string{"status.example.com"},
		LastError:     "http-01 校验失败：请确认 TCP 80 可达",
		LastAttemptAt: time.Now().Unix(),
	})
	err := runTLS([]string{"status", "-config", cfg})
	if !errors.Is(err, errCertFailed) {
		t.Fatalf("本轮申请失败时应返回 errCertFailed，实际 %v", err)
	}
}

// TestTLSStatusIgnoresStaleFailure 是这条命令最容易出错的地方。
//
// 服务上一次启动留下的失败原因会一直躺在状态文件里。若不加"是否本轮"的判断，
// 一次成功部署（或一次仍在进行中的申请）会被报成"申请失败"——
// 而用户会照着这个错误的结论去改配置。
func TestTLSStatusIgnoresStaleFailure(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTLSConfig(t, dir, acmeTLSBlock)
	loaded := loadCfgForTLS(t, cfg)
	writeStatus(t, tlsconf.StatusPath(loaded.TLS, loaded.DataDir), tlsconf.Status{
		Mode: "acme", Challenge: "http-01", Domains: []string{"status.example.com"},
		LastError:     "上一次启动时的旧错误",
		LastAttemptAt: time.Now().Add(-2 * time.Hour).Unix(),
	})
	err := runTLS([]string{"status", "-config", cfg, "-wait", "1"})
	if errors.Is(err, errCertFailed) {
		t.Fatal("把历史遗留的旧错误当成了本轮失败——这会让成功部署被报成失败")
	}
	if !errors.Is(err, errCertPending) {
		t.Fatalf("尚无结果时应返回 errCertPending，实际 %v", err)
	}
}

// TestTLSStatusNoStatusFile：状态文件都不存在时，退出码应为"还没结果"（2），
// 而不是失败（1）——服务只是还没跑起来而已。
func TestTLSStatusNoStatusFile(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTLSConfig(t, dir, acmeTLSBlock)
	err := runTLS([]string{"status", "-config", cfg, "-wait", "1"})
	if !errors.Is(err, errCertPending) {
		t.Fatalf("没有状态文件时应返回 errCertPending，实际 %v", err)
	}
}

// TestTLSStatusNoneMode：tls.mode=none 时不该去读状态文件（也不该报错）。
func TestTLSStatusNoneMode(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTLSConfig(t, dir, "tls:\n  mode: none\n")
	if err := runTLS([]string{"status", "-config", cfg}); err != nil {
		t.Fatalf("mode=none 时应直接返回成功（证书由反代负责）：%v", err)
	}
}

// TestTLSStatusUnknownSubcommand：写错的子命令要明确报错，而不是被当成配置路径。
func TestTLSStatusUnknownSubcommand(t *testing.T) {
	if err := runTLS([]string{"issue"}); err == nil {
		t.Fatal("未知子命令应当报错")
	}
}
