package vmqfox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/common"
	"github.com/shopspring/decimal"
)

var (
	ErrConfigInvalid    = errors.New("vmqfox config invalid")
	ErrRequestFailed    = errors.New("vmqfox request failed")
	ErrResponseInvalid  = errors.New("vmqfox response invalid")
	ErrSignatureInvalid = errors.New("vmqfox signature invalid")
)

type Config struct {
	GatewayURL  string `json:"gateway_url"`
	MerchantKey string `json:"merchant_key"`
	NotifyURL   string `json:"notify_url"`
	ReturnURL   string `json:"return_url"`
}

func ParseConfig(raw map[string]interface{}) (*Config, error) {
	return common.ParseConfig[Config](raw, ErrConfigInvalid)
}

func (c *Config) Normalize() {
	c.GatewayURL = strings.TrimRight(strings.TrimSpace(c.GatewayURL), "/")
	c.NotifyURL = strings.TrimSpace(c.NotifyURL)
	c.ReturnURL = strings.TrimSpace(c.ReturnURL)
	// The communication key is opaque: whitespace can be part of the key.
}

func ValidateConfig(cfg *Config) error {
	if cfg == nil || strings.TrimSpace(cfg.MerchantKey) == "" {
		return fmt.Errorf("%w: merchant_key is required", ErrConfigInvalid)
	}
	for field, raw := range map[string]string{
		"gateway_url": cfg.GatewayURL, "notify_url": cfg.NotifyURL, "return_url": cfg.ReturnURL,
	} {
		if !validHTTPURL(raw) {
			return fmt.Errorf("%w: invalid %s", ErrConfigInvalid, field)
		}
	}
	gateway, _ := url.Parse(cfg.GatewayURL)
	if gateway.RawQuery != "" || gateway.Fragment != "" {
		return fmt.Errorf("%w: gateway_url must not contain a query or fragment", ErrConfigInvalid)
	}
	return nil
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") &&
		u.Hostname() != "" && u.User == nil
}

func PaymentType(channelType string) string {
	switch strings.ToLower(strings.TrimSpace(channelType)) {
	case constants.PaymentChannelTypeWechat, "wxpay":
		return "1"
	case constants.PaymentChannelTypeAlipay:
		return "2"
	default:
		return ""
	}
}

type CreateInput struct {
	OrderNo     string
	Amount      string
	ChannelType string
	NotifyURL   string
	ReturnURL   string
}

type CreateResult struct {
	OrderID     string
	RedirectURL string
	Raw         map[string]interface{}
}

// CreateSign and CallbackSign deliberately preserve field order and raw text.
// vmqfox v2 does not sort or URL-encode the canonical string.
func CreateSign(values url.Values, key string) string {
	return sign("payId="+values.Get("payId")+"&param="+values.Get("param")+
		"&type="+values.Get("type")+"&price="+values.Get("price")+
		"&notifyUrl="+values.Get("notifyUrl")+"&returnUrl="+values.Get("returnUrl"), key)
}

func CallbackSign(values url.Values, key string) string {
	return sign("payId="+values.Get("payId")+"&param="+values.Get("param")+
		"&type="+values.Get("type")+"&price="+values.Get("price")+
		"&reallyPrice="+values.Get("reallyPrice"), key)
}

func sign(canonical, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

func CreatePayment(ctx context.Context, cfg *Config, input CreateInput) (*CreateResult, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	payType := PaymentType(input.ChannelType)
	amount, err := decimal.NewFromString(input.Amount)
	if err != nil || !amount.IsPositive() || !amount.Equal(amount.Round(2)) ||
		payType == "" || strings.TrimSpace(input.OrderNo) == "" {
		return nil, fmt.Errorf("%w: invalid order, amount or channel_type", ErrConfigInvalid)
	}
	notifyURL, returnURL := input.NotifyURL, input.ReturnURL
	if notifyURL == "" {
		notifyURL = cfg.NotifyURL
	}
	if returnURL == "" {
		returnURL = cfg.ReturnURL
	}
	if !validHTTPURL(notifyURL) || !validHTTPURL(returnURL) {
		return nil, fmt.Errorf("%w: invalid callback URL", ErrConfigInvalid)
	}
	values := url.Values{
		"payId": {input.OrderNo}, "param": {""}, "type": {payType},
		"price": {amount.StringFixed(2)}, "notifyUrl": {notifyURL}, "returnUrl": {returnURL},
	}
	values.Set("sign", CreateSign(values, cfg.MerchantKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.GatewayURL+"/api/order/create",
		strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrRequestFailed)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrRequestFailed, resp.StatusCode)
	}
	const maxResponseBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: read response", ErrResponseInvalid)
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			PayID       string `json:"payId"`
			OrderID     string `json:"orderId"`
			PayType     int    `json:"payType"`
			Price       string `json:"price"`
			ReallyPrice string `json:"reallyPrice"`
			RedirectURL string `json:"redirectUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Code != 200 {
		return nil, fmt.Errorf("%w: gateway rejected order or returned invalid JSON", ErrResponseInvalid)
	}
	data := envelope.Data
	if data.PayID != input.OrderNo || fmt.Sprint(data.PayType) != payType ||
		data.Price != values.Get("price") || data.OrderID == "" ||
		!validHTTPURL(data.RedirectURL) || !validPrice(data.ReallyPrice) {
		return nil, fmt.Errorf("%w: missing or mismatched order data", ErrResponseInvalid)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, ErrResponseInvalid
	}
	return &CreateResult{OrderID: data.OrderID, RedirectURL: data.RedirectURL, Raw: raw}, nil
}

func validPrice(raw string) bool {
	if len(raw) < 4 || len(raw) > 32 || raw[len(raw)-3] != '.' {
		return false
	}
	for i, ch := range raw {
		if i != len(raw)-3 && (ch < '0' || ch > '9') {
			return false
		}
	}
	amount, err := decimal.NewFromString(raw)
	return err == nil && amount.IsPositive() && amount.StringFixed(2) == raw
}

func VerifyCallback(cfg *Config, form url.Values) error {
	if cfg == nil || strings.TrimSpace(cfg.MerchantKey) == "" {
		return ErrConfigInvalid
	}
	for _, key := range []string{"payId", "param", "type", "price", "reallyPrice", "sign"} {
		if len(form[key]) != 1 {
			return fmt.Errorf("%w: missing or repeated %s", ErrSignatureInvalid, key)
		}
	}
	if form.Get("payId") == "" || form.Get("param") != "" ||
		(form.Get("type") != "1" && form.Get("type") != "2") ||
		!validPrice(form.Get("price")) || !validPrice(form.Get("reallyPrice")) {
		return ErrSignatureInvalid
	}
	expected := CallbackSign(form, cfg.MerchantKey)
	if !hmac.Equal([]byte(form.Get("sign")), []byte(expected)) {
		return ErrSignatureInvalid
	}
	return nil
}
