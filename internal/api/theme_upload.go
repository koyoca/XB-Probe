package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/theme"
)

// 主题包上传（POST /api/admin/v1/themes/upload，multipart/form-data，字段名 file）。
//
// 约定：**zip 文件名去掉 .zip 就是主题名**；同名视为同一主题的新版本，整目录替换。
// 校验与解压逻辑在 theme.InstallZip（zip-slip、体积上限、缺 theme.json 都在那边挡住），
// 这里只负责收包、落临时文件、决定是否顺手切换。
//
// activate=1 时安装完立即切过去：常见操作就是"传一个新主题并马上看效果"，
// 分成两步反而要多点一次。
func (h *Handler) handleUploadTheme(w http.ResponseWriter, r *http.Request) {
	if h.Themes == nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "主题管理器未初始化")
		return
	}
	// ParseMultipartForm 的入参是"内存里最多留多少"，其余落临时文件。
	// 上限已在 BodyLimit 里按 theme.MaxZipBytes 放宽。
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam,
			fmt.Sprintf("主题包解析失败（需 multipart/form-data，字段名 file，上限 %d MB）", theme.MaxZipBytes>>20))
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "缺少文件字段 file")
		return
	}
	defer f.Close()

	// 只取basename：某些客户端会把完整路径塞进 filename。
	// 主题名 = 文件名去扩展名，中文名可用（目录名在 Linux/Windows 都合法）。
	base := filepath.Base(hdr.Filename)
	name := strings.TrimSuffix(strings.TrimSuffix(base, ".zip"), ".ZIP")

	tmp, err := os.CreateTemp("", "xb-theme-*.zip")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "创建临时文件失败")
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	n, err := io.Copy(tmp, io.LimitReader(f, theme.MaxZipBytes+1))
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "接收主题包失败")
		return
	}
	if n > theme.MaxZipBytes {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam,
			fmt.Sprintf("主题包过大（上限 %d MB）", theme.MaxZipBytes>>20))
		return
	}

	installed, err := h.Themes.InstallZip(tmpPath, name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
		return
	}

	activated := false
	if r.URL.Query().Get("activate") == "1" {
		if err := h.Themes.SetActive(installed); err != nil {
			// 装成功了但切不过去：仍然算部分成功，把原因带回前端。
			writeJSON(w, http.StatusOK, map[string]any{
				"name": installed, "activated": false,
				"warning": "主题已安装，但切换失败：" + err.Error(),
			})
			return
		}
		if err := h.Repo.SetSetting("theme.active", installed, time.Now().Unix()); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"name": installed, "activated": false,
				"warning": "主题已安装并生效于内存，但保存设置失败，重启后会回退",
			})
			return
		}
		activated = true
	}

	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "theme.upload",
		installed, RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"name": installed, "activated": activated,
	})
}
