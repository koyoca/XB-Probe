// Command dbfp 打印一个 probe.db 的"数据指纹"，供备份恢复演练比对。
//
// 为什么要有这个工具，而不是在 shell 里用 sqlite3：
//   - 本项目**不假设目标机装了 sqlite3 CLI**（Windows 与极简容器上很常见）；
//   - 演练脚本需要在"恢复前主库"与"恢复后主库"之间做**可重复的**比对，
//     如果靠一堆 curl + grep 间接推断，很容易把"数据其实丢了"测成通过。
//
// 指纹 = 每个表的行数 + 若干关键业务值，按稳定的表格顺序逐行输出为
// "<名称> <值>"，让调用方可以直接 diff/比较字符串。
//
// 它是演练/排障工具，不是产品的一部分：只在 scripts/restore-drill.sh 里被调用。
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// 表顺序固定：先配置类（必须逐字节等价），再时序类（只要求不减少）。
var tables = []string{
	// 配置类
	"settings", "admins", "sessions", "nodes", "node_tokens", "enroll_codes",
	"probe_targets", "node_billing", "alert_channels", "alert_rules",
	"traffic_state", "traffic_daily", "traffic_nic_state",
	// 时序类
	"metrics_raw", "metrics_1m", "metrics_10m",
	"latency_raw", "latency_10m",
	// 审计
	"audit_log",
}

func main() {
	dbPath := flag.String("db", "", "probe.db 路径")
	flag.Parse()
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "用法：dbfp -db <probe.db>")
		os.Exit(2)
	}

	// 只读打开：演练里绝不能因为"看一眼指纹"而改到库。
	dsn := "file:" + *dbPath + "?_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开失败：", err)
		os.Exit(1)
	}
	defer db.Close()

	// 库都打不开/不是 SQLite 时直接报错，别输出半张指纹表单——
	// 调用方拿到"不完整但看起来正常"的指纹会 misjudge。
	if _, err := db.Exec(`SELECT count(*) FROM sqlite_master`); err != nil {
		fmt.Fprintln(os.Stderr, "不是有效的 SQLite 库：", err)
		os.Exit(1)
	}

	var b strings.Builder
	for _, t := range tables {
		var n int64
		// 表可能因迁移版本缺失而不存在：跳过而不是报错。
		if err := db.QueryRow(`SELECT count(*) FROM ` + t).Scan(&n); err != nil {
			continue
		}
		fmt.Fprintf(&b, "%s %d\n", t, n)
	}

	// 关键业务值：行数相同不代表内容相同（比如恢复了另一个空库的 schema）。
	var v string
	if err := db.QueryRow(`SELECT COALESCE(value,'') FROM settings WHERE key='public.enabled'`).Scan(&v); err == nil {
		fmt.Fprintf(&b, "kv.public.enabled %s\n", v)
	}
	if err := db.QueryRow(`SELECT COALESCE(MAX(name),'') FROM nodes`).Scan(&v); err == nil {
		fmt.Fprintf(&b, "kv.max_node_name %s\n", v)
	}
	// 管理员数量：恢复后必须还能登录，数量为 0 就是灾难。
	var admins int64
	if err := db.QueryRow(`SELECT count(*) FROM admins`).Scan(&admins); err == nil {
		fmt.Fprintf(&b, "kv.admin_count %d\n", admins)
	}
	// 计费价格（分）：验证整数型业务字段没被截断。
	var price int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(price_minor),0) FROM node_billing`).Scan(&price); err == nil {
		fmt.Fprintf(&b, "kv.billing_sum_minor %d\n", price)
	}

	os.Stdout.WriteString(b.String())
}
