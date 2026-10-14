package api

import (
	"embed"
	"html"
	"io/fs"
	"net/http"
	"strings"
)

// 管理后台前端（固定界面，5.2）。
//
// 内嵌进 Master 单二进制，因此无需任何外部文件即可运行；界面不随公开主题变化。
// 静态资源本身不含敏感数据（真正的鉴权由页面发起的 /api/admin/v1 请求承担），
// 所以可以免登录下发；但 CSP 恒为最严（SecurityHeaders(nil, ...)）。
//
//go:embed adminui
var adminUIFS embed.FS

// handleAdminUI 提供 /admin/ 下的后台界面与静态资源。
func (h *Handler) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/admin/")
	if rel == "" {
		rel = "index.html"
	}
	// 防路径穿越：规范化后必须仍在 adminui 内。
	clean := strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
	if clean == "" || strings.Contains(clean, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(adminUIFS, "adminui/"+clean)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 站点标题：登录页是最早出现的画面，那时前端还没登录、拿不到 /settings，
	// 所以标题只能由服务端在**下发这一刻**填进去（模板占位 __SITE_TITLE__）。
	// 以前这里写死 "XB-Probe"：管理员把站点标题改成自己的名字后，公开看板已经是
	// 新名字，登录页却还挂着 XB-Probe——看上去像"设置没生效"。
	//
	// 只替换 index.html：css/js 里没有这个占位符，其他静态资源不必过一遍替换。
	if clean == "index.html" {
		title := strings.TrimSpace(h.Repo.GetSettingDefault("site.title", "XB-Probe"))
		if title == "" {
			title = "XB-Probe"
		}
		data = []byte(strings.ReplaceAll(string(data), "__SITE_TITLE__", html.EscapeString(title)))
	}
	w.Header().Set("Content-Type", adminContentType(clean))
	// 后台界面不缓存，避免升级后仍加载旧资源。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func adminContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}
