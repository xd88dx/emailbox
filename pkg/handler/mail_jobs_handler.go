package handler

import (
	"net/http"

	"emailbox/pkg/middleware"
	"emailbox/pkg/service"

	"github.com/labstack/echo/v5"
)

// MailJobHandler 是收信检查与批量已读的入口。
//
// 两者都是后台任务:账号多的时候一次要跑几分钟,同步等待会撞上 HTTP 超时,
// 浏览器一刷新进度也就丢了。提交后由前端订阅 /jobs/:jobID/stream。
type MailJobHandler struct {
	unread   *service.UnreadService
	markRead *service.MarkReadService
}

func NewMailJobHandler(unread *service.UnreadService, markRead *service.MarkReadService) *MailJobHandler {
	return &MailJobHandler{unread: unread, markRead: markRead}
}

type submitUnreadRequest struct {
	Scope      string   `json:"scope"`
	AccountIDs []string `json:"account_ids"`
	GroupIDs   []string `json:"group_ids"`
}

func (h *MailJobHandler) SubmitUnread(c *echo.Context) error {
	var req submitUnreadRequest
	if err := c.Bind(&req); err != nil {
		return failure(c, http.StatusBadRequest, err)
	}
	job, err := h.unread.SubmitBatch(c.Request().Context(),
		c.Param("tenantID"), middleware.UserID(c), req.Scope, req.AccountIDs, req.GroupIDs)
	if err != nil {
		return mailError(c, err)
	}
	return success(c, job, "任务已提交")
}

type submitMarkReadRequest struct {
	AccountIDs []string `json:"account_ids"`
}

func (h *MailJobHandler) SubmitMarkRead(c *echo.Context) error {
	var req submitMarkReadRequest
	if err := c.Bind(&req); err != nil {
		return failure(c, http.StatusBadRequest, err)
	}
	job, err := h.markRead.SubmitBatch(c.Request().Context(),
		c.Param("tenantID"), middleware.UserID(c), req.AccountIDs)
	if err != nil {
		return mailError(c, err)
	}
	return success(c, job, "任务已提交")
}
