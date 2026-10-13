package theme

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// 主题系统（文档 5.3）。
//
//   - 内置主题：二进制内嵌，无外部文件也能运行（默认 builtin）。
//   - 外部主题：读取 themesDir/<name>/，包含 theme.json、index.html 与静态资源。
//   - 安全：静态文件服务严格限制在主题目录内（防路径穿越）；主题只能通过公开 API 取数，
//     无权访问管理 API。

//go:embed builtin
var builtinFS embed.FS

// Meta 是 theme.json 的结构。
type Meta struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	APIVersion  string `json:"api_version"` // 所需公开 API 版本
	Entry       string `json:"entry"`       // 入口文件，默认 index.html
	Author      string `json:"author,omitempty"`
	Description string `json:"description,omitempty"`
}

// Info 是主题列表项。
type Info struct {
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
	Meta    Meta   `json:"meta"`
}

// Manager 管理主题的加载与静态文件读取。
type Manager struct {
	dir    string
	active string
	themes map[string]*loaded
}

type loaded struct {
	info Info
	// fsys 是主题根目录（内置为 embed FS 的子目录，外部为 os.DirFS）。
	fsys fs.FS
}

// NewManager 扫描 themesDir 并加载内置主题。dir 可为空（只用内置主题）。
func NewManager(dir string) (*Manager, error) {
	m := &Manager{dir: dir, active: "builtin", themes: map[string]*loaded{}}

	sub, err := fs.Sub(builtinFS, "builtin")
	if err != nil {
		return nil, fmt.Errorf("加载内置主题失败: %w", err)
	}
	m.themes["builtin"] = &loaded{
		info: Info{Name: "builtin", Builtin: true, Meta: Meta{Name: "Builtin", Version: "1.0.0", APIVersion: "v1", Entry: "index.html"}},
		fsys: sub,
	}

	if dir != "" {
		if err := m.scanExternal(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Manager) scanExternal() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "builtin" {
			continue
		}
		metaPath := filepath.Join(m.dir, e.Name(), "theme.json")
		raw, err := os.ReadFile(metaPath)
		if err != nil {
			continue // 无 theme.json 视为非主题目录
		}
		var meta Meta
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		if meta.Entry == "" {
			meta.Entry = "index.html"
		}
		if meta.Name == "" {
			meta.Name = e.Name()
		}
		m.themes[e.Name()] = &loaded{
			info: Info{Name: e.Name(), Builtin: false, Meta: meta},
			fsys: os.DirFS(filepath.Join(m.dir, e.Name())),
		}
	}
	return nil
}

// List 返回全部主题（内置在前，其余按名称排序）。
func (m *Manager) List() []Info {
	out := make([]Info, 0, len(m.themes))
	for _, t := range m.themes {
		out = append(out, t.info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Builtin != out[j].Builtin {
			return out[i].Builtin
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Exists 判断主题是否存在。
func (m *Manager) Exists(name string) bool {
	_, ok := m.themes[name]
	return ok
}

// SetActive 切换当前主题（热切换，无需重启，5.3）。未知主题返回错误。
func (m *Manager) SetActive(name string) error {
	if !m.Exists(name) {
		return fmt.Errorf("主题不存在：%s", name)
	}
	m.active = name
	return nil
}

// Active 返回当前主题名。
func (m *Manager) Active() string { return m.active }

// Open 读取主题内文件。
// 安全：请求路径先 Clean 再校验不含 ".."，且必须落在主题根内（防路径穿越）。
func (m *Manager) Open(name, rel string) ([]byte, error) {
	t, ok := m.themes[name]
	if !ok {
		return nil, fmt.Errorf("主题不存在")
	}
	p, err := safeJoin(rel)
	if err != nil {
		return nil, err
	}
	return fs.ReadFile(t.fsys, p)
}

// Entry 返回主题入口文件内容。
func (m *Manager) Entry(name string) ([]byte, error) {
	t, ok := m.themes[name]
	if !ok {
		return nil, fmt.Errorf("主题不存在")
	}
	entry := t.info.Meta.Entry
	if entry == "" {
		entry = "index.html"
	}
	p, err := safeJoin(entry)
	if err != nil {
		return nil, err
	}
	return fs.ReadFile(t.fsys, p)
}

// safeJoin 规范化相对路径并拒绝越界访问。
func safeJoin(rel string) (string, error) {
	rel = strings.TrimPrefix(rel, "/")
	// 统一分隔符并清理，随后拒绝任何上跳。
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" {
		clean = "index.html"
	}
	if strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("非法路径")
	}
	// 二次校验：join 后必须仍在根内。
	if full := path.Join(".", clean); strings.HasPrefix(full, "..") {
		return "", fmt.Errorf("非法路径")
	}
	return clean, nil
}

// Inject 把注入内容插入 HTML 入口（5.3 自定义注入）。
// head 片段插入 </head> 前，bodyEnd 插入 </body> 前；脚本注入默认关闭由调用方控制。
func Inject(html []byte, head, bodyEnd, nonce string) []byte {
	s := string(html)
	if head != "" {
		s = strings.Replace(s, "</head>", head+"\n</head>", 1)
	}
	if bodyEnd != "" {
		if nonce != "" {
			bodyEnd = strings.ReplaceAll(bodyEnd, "{{NONCE}}", nonce)
		}
		s = strings.Replace(s, "</body>", bodyEnd+"\n</body>", 1)
	}
	return []byte(s)
}
