package vmqfox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testConfig(gateway string) *Config {
	return &Config{
		GatewayURL: gateway, MerchantKey: "testkey123456",
		NotifyURL: "https://shop.example.com/notify", ReturnURL: "https://shop.example.com/return",
	}
}

func cloneForm(values url.Values) url.Values {
	copy, _ := url.ParseQuery(values.Encode())
	return copy
}

func TestV2GoldenVectors(t *testing.T) {
	values := url.Values{
		"payId": {"TEST20260314001"}, "param": {""}, "type": {"1"},
		"price": {"1.00"}, "reallyPrice": {"0.99"},
		"notifyUrl": {"https://shop.example.com/notify"}, "returnUrl": {"https://shop.example.com/return"},
	}
	// These vectors come from vmqfox's Go/PHP/Android protocol tests.
	if got := CreateSign(values, "testkey123456"); got != "729a6c529b4a2ffed215d124a7e4244ed5d4981ba1982d4cd6f9a53b28de9263" {
		t.Fatalf("create golden vector mismatch: %s", got)
	}
	if got := CallbackSign(values, "testkey123456"); got != "0f21b9366a12396a71437d336a21fd7c5fe20b292e9b56d25b6b612a144daa60" {
		t.Fatalf("callback golden vector mismatch: %s", got)
	}
	original := CreateSign(values, "testkey123456")
	for _, field := range []string{"payId", "param", "type", "price", "notifyUrl", "returnUrl"} {
		changed := cloneForm(values)
		changed.Set(field, changed.Get(field)+"x")
		if CreateSign(changed, "testkey123456") == original {
			t.Fatalf("%s is outside the create signature", field)
		}
	}
}

func TestCreatePaymentPOSTAndResponse(t *testing.T) {
	for _, channel := range []string{"wechat", "wxpay", "alipay"} {
		t.Run(channel, func(t *testing.T) {
			cfg := testConfig("")
			cfg.MerchantKey = " key with surrounding spaces "
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/order/create" || r.URL.RawQuery != "" {
					t.Errorf("expected server-side form POST, got %s %s", r.Method, r.URL)
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("sign") != CreateSign(r.Form, cfg.MerchantKey) {
					t.Error("invalid signature")
				}
				if r.Form.Get("type") != PaymentType(channel) || r.Form.Get("price") != "1.00" ||
					r.Form.Get("notifyUrl") != cfg.NotifyURL || r.Form.Get("returnUrl") != cfg.ReturnURL+"?vmqfox_return=1" {
					t.Errorf("unexpected request fields: %v", r.Form)
				}
				payType := 1
				if channel == "alipay" {
					payType = 2
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"code": 200, "data": map[string]interface{}{
						"payId": "DJP1", "orderId": "FOX1", "payType": payType,
						"price": "1.00", "reallyPrice": "0.99",
						"redirectUrl": "https://pay.example.com/#/payment/public-token",
					},
				})
			}))
			defer server.Close()
			cfg.GatewayURL = server.URL
			result, err := CreatePayment(context.Background(), cfg, CreateInput{
				OrderNo: "DJP1", Amount: "1", ChannelType: channel,
				ReturnURL: cfg.ReturnURL + "?vmqfox_return=1",
			})
			if err != nil || result.OrderID != "FOX1" || !strings.Contains(result.RedirectURL, "/#/payment/") {
				t.Fatalf("unexpected result: %+v, %v", result, err)
			}
		})
	}
}

func TestCreatePaymentRejectsInvalidResponses(t *testing.T) {
	valid := `{"code":200,"data":{"payId":"DJP1","orderId":"FOX1","payType":2,"price":"1.00","reallyPrice":"0.99","redirectUrl":"https://pay.example.com/#/payment/token"}}`
	for name, body := range map[string]string{
		"gateway failure": `{"code":400,"msg":"offline"}`,
		"invalid json":    `<html>bad gateway</html>`,
		"wrong order":     strings.Replace(valid, "DJP1", "OTHER", 1),
		"wrong amount":    strings.Replace(valid, `"price":"1.00"`, `"price":"2.00"`, 1),
		"wrong type":      strings.Replace(valid, `"payType":2`, `"payType":1`, 1),
		"unsafe redirect": strings.Replace(valid, "https://pay.example.com/#/payment/token", "javascript:alert(1)", 1),
		"missing cashier": strings.Replace(valid, "https://pay.example.com/#/payment/token", "", 1),
		"invalid actual":  strings.Replace(valid, "0.99", "1e2147483647", 1),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			_, err := CreatePayment(context.Background(), testConfig(server.URL),
				CreateInput{OrderNo: "DJP1", Amount: "1.00", ChannelType: "alipay"})
			if !errors.Is(err, ErrResponseInvalid) {
				t.Fatalf("expected response error, got %v", err)
			}
		})
	}
}

func TestCreatePaymentDoesNotFollowRedirects(t *testing.T) {
	followed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			followed = true
		}
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := CreatePayment(context.Background(), testConfig(server.URL),
		CreateInput{OrderNo: "DJP1", Amount: "1.00", ChannelType: "alipay"})
	if !errors.Is(err, ErrRequestFailed) || followed {
		t.Fatalf("unexpected redirect handling: followed=%v, err=%v", followed, err)
	}
}

func TestVerifyCallbackRejectsTampering(t *testing.T) {
	cfg := testConfig("https://pay.example.com")
	valid := url.Values{
		"payId": {"DJP1"}, "param": {""}, "type": {"1"}, "price": {"1.00"}, "reallyPrice": {"0.99"},
	}
	valid.Set("sign", CallbackSign(valid, cfg.MerchantKey))
	if err := VerifyCallback(cfg, valid); err != nil {
		t.Fatalf("valid callback rejected: %v", err)
	}
	for _, field := range []string{"payId", "param", "type", "price", "reallyPrice", "sign"} {
		t.Run(field, func(t *testing.T) {
			tampered := cloneForm(valid)
			tampered.Set(field, tampered.Get(field)+"x")
			if err := VerifyCallback(cfg, tampered); !errors.Is(err, ErrSignatureInvalid) {
				t.Fatalf("tampered %s accepted", field)
			}
			duplicate := cloneForm(valid)
			duplicate.Add(field, valid.Get(field))
			if err := VerifyCallback(cfg, duplicate); !errors.Is(err, ErrSignatureInvalid) {
				t.Fatalf("duplicate %s accepted", field)
			}
			missing := cloneForm(valid)
			missing.Del(field)
			if err := VerifyCallback(cfg, missing); !errors.Is(err, ErrSignatureInvalid) {
				t.Fatalf("missing %s accepted", field)
			}
		})
	}
}

func TestConfigAndPriceValidation(t *testing.T) {
	for _, raw := range []string{"", "0.00", "-1.00", "01.00", "1", "1.000", "1e2147483647", "1.0x"} {
		if validPrice(raw) {
			t.Errorf("invalid price accepted: %q", raw)
		}
	}
	for _, raw := range []string{"", "example.com", "ftp://pay.example.com", "https://key@pay.example.com", "https://pay.example.com?x=1", "https://pay.example.com/#/"} {
		if err := ValidateConfig(testConfig(raw)); !errors.Is(err, ErrConfigInvalid) {
			t.Errorf("invalid gateway URL accepted: %q", raw)
		}
	}
	cfg, err := ParseConfig(map[string]interface{}{
		"gateway_url": " https://pay.example.com/ ", "merchant_key": " exact key ",
		"notify_url": "https://shop.example.com/notify", "return_url": "https://shop.example.com/pay",
	})
	if err != nil || ValidateConfig(cfg) != nil || cfg.MerchantKey != " exact key " ||
		cfg.GatewayURL != "https://pay.example.com" {
		t.Fatalf("config normalization failed: %+v, %v", cfg, err)
	}
}
