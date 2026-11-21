// internal/routes/ghworkers_api.go
// Admin panel → Workers → "GitHub Actions Workspaces" card चे APIs.
// (adminAuthMiddleware + CSRF group मधूनच register होतात — registerGHWorkersAdmin(api) admin_api.go मधून call होतो.)
package routes

import (
	"net/http"

	"EverythingSuckz/fsb/internal/ghworkers"

	"github.com/gin-gonic/gin"
)

func registerGHWorkersAdmin(api *gin.RouterGroup) {
	api.GET("/ghworkers", adminGHGet)
	api.POST("/ghworkers/settings", adminGHSettings)
	api.POST("/ghworkers/workspace", adminGHAdd)
	api.PUT("/ghworkers/workspace/:id", adminGHUpdate)
	api.DELETE("/ghworkers/workspace/:id", adminGHDelete)
	api.POST("/ghworkers/workspace/:id/start", adminGHStart)
	api.POST("/ghworkers/workspace/:id/stop", adminGHStop)
	api.POST("/ghworkers/start-all", adminGHStartAll)
	api.POST("/ghworkers/stop-all", adminGHStopAll)
}

func ghFail(c *gin.Context, code int, err error) {
	c.JSON(code, gin.H{"ok": false, "error": err.Error()})
}

func adminGHGet(c *gin.Context) {
	snap, err := ghworkers.GetSnapshot()
	if err != nil {
		ghFail(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "snapshot": snap})
}

func adminGHSettings(c *gin.Context) {
	var req struct {
		AutoEnabled    *bool  `json:"auto_enabled"`
		DailyLimitMin  *int64 `json:"daily_limit_min"`
		RunLimitMin    *int64 `json:"run_limit_min"`
		RestartWaitMin *int64 `json:"restart_wait_min"`
		IdleOffMin     *int64 `json:"idle_off_min"`
		Parallel       *int64 `json:"parallel"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		ghFail(c, http.StatusBadRequest, err)
		return
	}
	if err := ghworkers.SaveSettings(req.AutoEnabled, req.DailyLimitMin, req.RunLimitMin, req.RestartWaitMin, req.IdleOffMin, req.Parallel); err != nil {
		ghFail(c, http.StatusBadRequest, err)
		return
	}
	adminGHGet(c)
}

func adminGHAdd(c *gin.Context) {
	var req struct {
		Label     string `json:"label"`
		Repo      string `json:"repo"`
		Token     string `json:"token"`
		EventType string `json:"event_type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		ghFail(c, http.StatusBadRequest, err)
		return
	}
	if _, err := ghworkers.AddWorkspace(req.Label, req.Repo, req.Token, req.EventType); err != nil {
		ghFail(c, http.StatusBadRequest, err)
		return
	}
	adminGHGet(c)
}

func adminGHUpdate(c *gin.Context) {
	var req struct {
		Enabled       *bool  `json:"enabled"`
		DailyLimitMin *int64 `json:"daily_limit_min"` // 0 = global default
	}
	if err := c.ShouldBindJSON(&req); err != nil || (req.Enabled == nil && req.DailyLimitMin == nil) {
		ghFail(c, http.StatusBadRequest, http.ErrNotSupported)
		return
	}
	id := c.Param("id")
	if req.DailyLimitMin != nil {
		if err := ghworkers.SetDailyLimit(id, *req.DailyLimitMin); err != nil {
			ghFail(c, http.StatusBadRequest, err)
			return
		}
	}
	if req.Enabled != nil {
		if err := ghworkers.SetEnabled(id, *req.Enabled); err != nil {
			ghFail(c, http.StatusInternalServerError, err)
			return
		}
	}
	adminGHGet(c)
}

func adminGHDelete(c *gin.Context) {
	if err := ghworkers.DeleteWorkspace(c.Param("id")); err != nil {
		ghFail(c, http.StatusInternalServerError, err)
		return
	}
	adminGHGet(c)
}

func adminGHStart(c *gin.Context) {
	if err := ghworkers.StartNow(c.Param("id")); err != nil {
		ghFail(c, http.StatusBadRequest, err)
		return
	}
	adminGHGet(c)
}

func adminGHStop(c *gin.Context) {
	if err := ghworkers.StopNow(c.Param("id")); err != nil {
		ghFail(c, http.StatusBadRequest, err)
		return
	}
	adminGHGet(c)
}

func adminGHStartAll(c *gin.Context) {
	n, errs := ghworkers.StartAll()
	snap, _ := ghworkers.GetSnapshot()
	c.JSON(http.StatusOK, gin.H{"ok": len(errs) == 0, "started": n, "errors": errs, "snapshot": snap})
}

func adminGHStopAll(c *gin.Context) {
	n, errs := ghworkers.StopAll()
	snap, _ := ghworkers.GetSnapshot()
	c.JSON(http.StatusOK, gin.H{"ok": len(errs) == 0, "stopped": n, "errors": errs, "snapshot": snap})
}
