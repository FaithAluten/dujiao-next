package vmqfoxadapter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dujiao-next/internal/constants"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/common"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/vmqfox"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

type adapter struct{}

var (
	_ paymentcontract.GatewayProvider         = (*adapter)(nil)
	_ paymentcontract.GatewayCallbackVerifier = (*adapter)(nil)
)

func NewVmqfoxAdapter() paymentcontract.GatewayProvider { return &adapter{} }

func (a *adapter) Type() string { return constants.PaymentProviderVmqfox + ":" }

func (a *adapter) ValidateConfig(raw jsonmap.JSON, channelType string) error {
	if vmqfox.PaymentType(channelType) == "" {
		return paymentcontract.ErrGatewayUnsupportedChannel
	}
	cfg, err := vmqfox.ParseConfig(raw)
	if err != nil {
		return mapError(err)
	}
	return mapError(vmqfox.ValidateConfig(cfg))
}

func (a *adapter) CreatePayment(ctx context.Context, raw jsonmap.JSON, input paymentcontract.GatewayCreateInput) (*paymentcontract.GatewayCreateResult, error) {
	if err := a.ValidateConfig(raw, input.ChannelType); err != nil {
		return nil, err
	}
	mode, _ := input.Extra["interaction_mode"].(string)
	if (mode != "" && mode != constants.PaymentInteractionRedirect) ||
		!strings.EqualFold(strings.TrimSpace(input.Currency), "CNY") {
		return nil, fmt.Errorf("%w: vmqfox requires CNY and redirect mode", paymentcontract.ErrGatewayConfigInvalid)
	}
	cfg, _ := vmqfox.ParseConfig(raw)
	returnURL := input.ReturnURL
	if returnURL == "" {
		returnURL = cfg.ReturnURL
	}
	result, err := vmqfox.CreatePayment(ctx, cfg, vmqfox.CreateInput{
		OrderNo: input.OrderNo, Amount: input.Amount.String(), ChannelType: input.ChannelType,
		NotifyURL: input.NotifyURL, ReturnURL: common.AppendQueryParams(returnURL, input.ReturnURLQuery),
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &paymentcontract.GatewayCreateResult{
		ProviderRef: result.OrderID, RedirectURL: result.RedirectURL,
		Payload: jsonmap.JSON(result.Raw), AmountSent: input.Amount.String(), CurrencySent: "CNY",
	}, nil
}

func (a *adapter) VerifyCallback(raw jsonmap.JSON, form map[string][]string, _ []byte) (*paymentcontract.GatewayCallbackResult, error) {
	cfg, err := vmqfox.ParseConfig(raw)
	if err != nil {
		return nil, mapError(err)
	}
	if err := vmqfox.VerifyCallback(cfg, form); err != nil {
		return nil, mapError(err)
	}
	// vmqfox's signed price is the merchant order amount. reallyPrice can be
	// adjusted by its price lock and is retained in the payload for reconciliation.
	amount, _ := decimal.NewFromString(common.FormValue(form, "price"))
	return &paymentcontract.GatewayCallbackResult{
		OrderNo: common.FormValue(form, "payId"), Status: constants.PaymentStatusSuccess,
		Amount: money.FromDecimal(amount), Currency: "CNY", Payload: common.FormToJSON(form),
	}, nil
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	target := paymentcontract.ErrGatewayResponseInvalid
	switch {
	case errors.Is(err, vmqfox.ErrConfigInvalid):
		target = paymentcontract.ErrGatewayConfigInvalid
	case errors.Is(err, vmqfox.ErrSignatureInvalid):
		target = paymentcontract.ErrGatewaySignatureInvalid
	case errors.Is(err, vmqfox.ErrRequestFailed):
		target = paymentcontract.ErrGatewayRequestFailed
	}
	return fmt.Errorf("%w: %v", target, err)
}
