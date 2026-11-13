package paymentcallbackhttp

import (
	"net/http"
	"strings"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/platform/http/ginutil"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/gin-gonic/gin"
)

func (h *Handler) handleVmqfoxCallback(c *gin.Context) bool {
	// vmqfox sends form POST notifications. Browser returns go to the storefront.
	if c.Request.Method != http.MethodPost {
		return false
	}
	form, err := parseCallbackForm(c)
	if err != nil || getFirstValue(form, "payId") == "" || len(form["reallyPrice"]) == 0 {
		return false
	}
	fail := func() bool {
		c.String(http.StatusOK, "fail")
		return true
	}
	payment, err := h.payments.GetByGatewayOrderNo(getFirstValue(form, "payId"))
	if err != nil || payment == nil || payment.ProviderType != constants.PaymentProviderVmqfox {
		return fail()
	}
	channel, err := h.channels.GetByID(payment.ChannelID)
	if err != nil || channel == nil || channel.ProviderType != constants.PaymentProviderVmqfox {
		return fail()
	}
	payType := ""
	switch strings.ToLower(strings.TrimSpace(channel.ChannelType)) {
	case constants.PaymentChannelTypeWechat, "wxpay":
		payType = "1"
	case constants.PaymentChannelTypeAlipay:
		payType = "2"
	}
	if payType == "" || getFirstValue(form, "type") != payType {
		return fail()
	}
	if _, err := h.service.HandleSyncCallback(channel, form, nil); err != nil {
		ginutil.RequestLog(c).Warnw("vmqfox_callback_handle_failed", "payment_id", payment.ID, "error", err)
		h.enqueuePaymentExceptionAlert(c, jsonmap.JSON{
			"alert_type": "vmqfox_callback_handle_failed", "alert_level": "error",
			"provider": constants.PaymentProviderVmqfox, "message": err.Error(),
		})
		return fail()
	}
	c.String(http.StatusOK, "success")
	return true
}
