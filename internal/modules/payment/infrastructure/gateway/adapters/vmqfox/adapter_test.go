package vmqfoxadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/dujiao-next/internal/constants"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/vmqfox"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

func config() jsonmap.JSON {
	return jsonmap.JSON{
		"gateway_url": "https://pay.example.com", "merchant_key": "test-key",
		"notify_url": "https://shop.example.com/api/v1/payments/callback",
		"return_url": "https://shop.example.com/pay",
	}
}

func TestValidateConfigAndCreateRestrictions(t *testing.T) {
	provider := NewVmqfoxAdapter()
	for _, channel := range []string{"wechat", "wxpay", "alipay"} {
		if err := provider.ValidateConfig(config(), channel); err != nil {
			t.Fatalf("valid channel %s rejected: %v", channel, err)
		}
	}
	if err := provider.ValidateConfig(config(), "qqpay"); !errors.Is(err, paymentcontract.ErrGatewayUnsupportedChannel) {
		t.Fatalf("qqpay must not be supported: %v", err)
	}
	for name, input := range map[string]paymentcontract.GatewayCreateInput{
		"non CNY": {ChannelType: "alipay", Currency: "USD"},
		"QR":      {ChannelType: "alipay", Currency: "CNY", Extra: jsonmap.JSON{"interaction_mode": "qr"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := provider.CreatePayment(context.Background(), config(), input); !errors.Is(err, paymentcontract.ErrGatewayConfigInvalid) {
				t.Fatalf("expected invalid config before HTTP request, got %v", err)
			}
		})
	}
}

func TestCallbackUsesSignedMerchantPrice(t *testing.T) {
	form := url.Values{
		"payId": {"DJP1"}, "param": {""}, "type": {"2"}, "price": {"10.00"}, "reallyPrice": {"9.99"},
	}
	form.Set("sign", vmqfox.CallbackSign(form, "test-key"))
	verifier := NewVmqfoxAdapter().(paymentcontract.GatewayCallbackVerifier)
	result, err := verifier.VerifyCallback(config(), form, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.OrderNo != "DJP1" || result.Status != constants.PaymentStatusSuccess ||
		result.Currency != "CNY" || result.Amount.String() != "10.00" ||
		result.Payload["reallyPrice"] != "9.99" || result.ProviderRef != "" {
		t.Fatalf("unexpected callback result: %+v", result)
	}
	form.Set("reallyPrice", "0.01")
	if _, err := verifier.VerifyCallback(config(), form, nil); !errors.Is(err, paymentcontract.ErrGatewaySignatureInvalid) {
		t.Fatalf("tampered actual amount accepted: %v", err)
	}
}

func TestCreatePaymentSignsReturnTrackingAndKeepsNominalAmount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("returnUrl") != "https://tenant.example.com/pay?biz_type=order&vmqfox_return=1" ||
			r.Form.Get("notifyUrl") != "https://shop.example.com/api/v1/payments/callback" ||
			r.Form.Get("sign") != vmqfox.CreateSign(r.Form, "test-key") {
			t.Errorf("incorrect signed callback URLs: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 200, "data": map[string]interface{}{
				"payId": "DJP1", "orderId": "FOX1", "payType": 2,
				"price": "10.00", "reallyPrice": "9.99",
				"redirectUrl": "https://pay.example.com/#/payment/token",
			},
		})
	}))
	defer server.Close()
	cfg := config()
	cfg["gateway_url"] = server.URL
	result, err := NewVmqfoxAdapter().CreatePayment(context.Background(), cfg, paymentcontract.GatewayCreateInput{
		OrderNo: "DJP1", ChannelType: "alipay", Currency: "CNY",
		Amount: money.FromDecimal(decimal.NewFromInt(10)), ReturnURL: "https://tenant.example.com/pay",
		ReturnURLQuery: map[string]string{"vmqfox_return": "1", "biz_type": "order"},
		Extra:          jsonmap.JSON{"interaction_mode": "redirect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AmountSent != "10.00" || result.CurrencySent != "CNY" ||
		result.ProviderRef != "FOX1" || result.RedirectURL == "" || result.QRCodeURL != "" {
		t.Fatalf("unexpected create result: %+v", result)
	}
}
