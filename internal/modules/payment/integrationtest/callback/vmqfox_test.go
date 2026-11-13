package paymentcallback_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productgormstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	ordergormstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	vmqfoxadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/vmqfox"
	paymentprovider "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/provider"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/vmqfox"
	paymentgormstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"
	paymentcallback "github.com/dujiao-next/internal/modules/payment/transport/http/callback"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestVmqfoxCallbackPaymentLifecycle(t *testing.T) {
	for _, name := range []string{
		"paid and repeated", "wechat paid", "invalid signature", "signed wrong price", "signed wrong type",
		"unknown order", "wrong provider", "duplicate amount", "GET is not a notification",
	} {
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:vmqfox_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(
				&userdomain.User{}, &productdomain.Product{}, &productdomain.ProductSKU{},
				&orderdomain.Order{}, &orderdomain.OrderItem{}, &fulfillmentdomain.Fulfillment{},
				&paymentdomain.PaymentChannel{}, &paymentdomain.Payment{},
			); err != nil {
				t.Fatal(err)
			}
			amount := money.FromDecimal(decimal.NewFromInt(10))
			order := &orderdomain.Order{
				OrderNo: "DJ1", Status: constants.OrderStatusPendingPayment,
				Currency: "CNY", TotalAmount: amount, OriginalAmount: amount, OnlinePaidAmount: amount,
			}
			channel := &paymentdomain.PaymentChannel{
				Name: "Vmqfox", ProviderType: constants.PaymentProviderVmqfox,
				ChannelType: constants.PaymentChannelTypeAlipay, InteractionMode: constants.PaymentInteractionRedirect,
				ConfigJSON: jsonmap.JSON{"merchant_key": "test-key"}, IsActive: true,
			}
			if name == "wechat paid" {
				channel.ChannelType = constants.PaymentChannelTypeWechat
			}
			for _, value := range []interface{}{order, channel} {
				if err := db.Create(value).Error; err != nil {
					t.Fatal(err)
				}
			}
			payment := &paymentdomain.Payment{
				OrderID: order.ID, ChannelID: channel.ID, ProviderType: channel.ProviderType,
				ChannelType: channel.ChannelType, InteractionMode: channel.InteractionMode,
				Amount: amount, Currency: "CNY", Status: constants.PaymentStatusPending,
				GatewayOrderNo: "DJP1", ProviderRef: "FOX1",
			}
			if name == "wrong provider" {
				payment.ProviderType = constants.PaymentProviderEpay
			}
			if err := db.Create(payment).Error; err != nil {
				t.Fatal(err)
			}
			orderRepo := ordergormstore.New(db, "test-guest-credential-secret-with-32-bytes")
			paymentRepo := paymentgormstore.New(db, "test-guest-credential-secret-with-32-bytes")
			channelRepo := paymentgormstore.NewChannelStore(db)
			registry := paymentprovider.NewRegistry()
			registry.Register(constants.PaymentProviderVmqfox, "", vmqfoxadapter.NewVmqfoxAdapter())
			service := paymentapp.NewPaymentService(paymentapp.PaymentServiceOptions{
				OrderStore: orderRepo, PaymentStore: paymentRepo, ChannelStore: channelRepo,
				ProductRepo: productgormstore.NewProductStore(db), ProductSKURepo: productgormstore.NewSKUStore(db),
				PaymentProviderRegistry: registry, ExpireMinutes: 15,
			})
			handler := paymentcallback.NewHandler(callbackServiceTestAdapter{payments: service}, paymentRepo, channelRepo, nil)
			form := url.Values{
				"payId": {"DJP1"}, "param": {""}, "type": {"2"}, "price": {"10.00"}, "reallyPrice": {"9.99"},
			}
			switch name {
			case "wechat paid":
				form.Set("type", "1")
			case "signed wrong price":
				form.Set("price", "0.01")
			case "signed wrong type":
				form.Set("type", "1")
			case "unknown order":
				form.Set("payId", "DJP2")
			case "duplicate amount":
				form.Add("price", "0.01")
			}
			form.Set("sign", vmqfox.CallbackSign(form, "test-key"))
			if name == "invalid signature" {
				form.Set("sign", strings.Repeat("0", 64))
			}
			count := 1
			if name == "paid and repeated" {
				count = 2
			}
			for range count {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/payments/callback", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if name == "GET is not a notification" {
					req = httptest.NewRequest(http.MethodGet, "/api/v1/payments/callback?"+form.Encode(), nil)
				}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = req
				handler.PaymentCallback(c)
				want := "fail"
				if name == "paid and repeated" || name == "wechat paid" {
					want = "success"
				}
				if name == "GET is not a notification" {
					if w.Code != http.StatusNotFound {
						t.Fatalf("GET callback unexpectedly recognized: %d %s", w.Code, w.Body.String())
					}
				} else if w.Code != http.StatusOK || w.Body.String() != want {
					t.Fatalf("callback returned %d %q, want %q", w.Code, w.Body.String(), want)
				}
			}
			updated, err := paymentRepo.GetByID(payment.ID)
			if err != nil {
				t.Fatal(err)
			}
			updatedOrder, err := orderRepo.GetByID(order.ID)
			if err != nil {
				t.Fatal(err)
			}
			if name == "paid and repeated" || name == "wechat paid" {
				if updated.Status != constants.PaymentStatusSuccess || updatedOrder.Status != constants.OrderStatusPaid ||
					updated.ProviderRef != "FOX1" || updated.Amount.String() != "10.00" ||
					updated.ProviderPayload["reallyPrice"] != "9.99" {
					t.Fatalf("payment lifecycle incorrect: payment=%+v order=%+v", updated, updatedOrder)
				}
			} else if updated.Status != constants.PaymentStatusPending || updatedOrder.Status != constants.OrderStatusPendingPayment {
				t.Fatalf("invalid callback changed payment or order: %+v %+v", updated, updatedOrder)
			}
		})
	}
}
