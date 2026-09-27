package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed web/index.html
var indexHTML []byte

type config struct {
	Port                    string
	DatabaseURL             string
	RedisAddr               string
	PaymentTimeoutMin       int     // 普通订单支付时限（分钟）
	PresalePaymentHours     int     // 节日预售锁货支付时限（小时）
	SweepIntervalSec        int     // 后台清扫周期（秒）
	StorageFeePerBundleHour float64 // 冷库费 元/扎/小时
	CreditDailyPenaltyRate  float64 // 账期逾期日罚息比例
	SeedDemo                bool    // 是否写入演示数据
}

type app struct {
	cfg config
	db  *sql.DB
	rdb *redis.Client
	mux *http.ServeMux
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func loadConfig() config {
	return config{
		Port:                    envStr("PORT", "8080"),
		DatabaseURL:             envStr("DATABASE_URL", "postgres://flower:flower@localhost:5432/flowermarket?sslmode=disable"),
		RedisAddr:               envStr("REDIS_ADDR", "localhost:6379"),
		PaymentTimeoutMin:       envInt("PAYMENT_TIMEOUT_MIN", 30),
		PresalePaymentHours:     envInt("PRESALE_PAYMENT_HOURS", 72),
		SweepIntervalSec:        envInt("SWEEP_INTERVAL_SEC", 15),
		StorageFeePerBundleHour: envFloat("STORAGE_FEE_PER_BUNDLE_HOUR", 0.2),
		CreditDailyPenaltyRate:  envFloat("CREDIT_DAILY_PENALTY_RATE", 0.005),
		SeedDemo:                envBool("SEED_DEMO", true),
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[flower] ")
	cfg := loadConfig()

	db := connectDB(cfg.DatabaseURL)
	defer db.Close()
	if err := migrate(db); err != nil {
		log.Fatalf("数据库迁移失败: %v", err)
	}
	rdb := connectRedis(cfg.RedisAddr)

	a := &app{cfg: cfg, db: db, rdb: rdb, mux: http.NewServeMux()}
	a.routes()

	a.syncFarmerRanking(context.Background())
	if cfg.SeedDemo {
		a.seedDemo()
	}
	go a.sweeper()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           a.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("凌晨鲜花竞价分货与损耗服务已启动，监听 :%s", cfg.Port)
	log.Fatal(srv.ListenAndServe())
}

func (a *app) routes() {
	a.mux.HandleFunc("GET /healthz", a.handleHealthz)
	a.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	// 认证
	a.mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	a.mux.HandleFunc("POST /api/auth/logout", a.auth(a.handleLogout))
	a.mux.HandleFunc("GET /api/auth/me", a.auth(a.handleMe))
	a.mux.HandleFunc("GET /api/farmers", a.auth(a.handleListFarmers))

	// 花材批次 / 质检 / 冷库 / 保鲜剂
	a.mux.HandleFunc("POST /api/batches", a.require("farmer", "broker")(a.handleCreateBatch))
	a.mux.HandleFunc("GET /api/batches", a.auth(a.handleListBatches))
	a.mux.HandleFunc("GET /api/batches/{id}", a.auth(a.handleGetBatch))
	a.mux.HandleFunc("GET /api/qc/pending", a.require("qc", "manager")(a.handleQCPending))
	a.mux.HandleFunc("POST /api/batches/{id}/qc", a.require("qc")(a.handleQC))
	a.mux.HandleFunc("GET /api/storage/slots", a.auth(a.handleListSlots))
	a.mux.HandleFunc("POST /api/batches/{id}/store", a.require("coldstore", "manager")(a.handleStoreBatch))
	a.mux.HandleFunc("GET /api/preservatives", a.auth(a.handleListPreservatives))
	a.mux.HandleFunc("POST /api/preservatives", a.require("coldstore", "qc", "manager")(a.handleCreatePreservative))
	a.mux.HandleFunc("POST /api/batches/{id}/preservative", a.require("coldstore", "qc")(a.handleApplyPreservative))

	// 凌晨竞价 / 一口价
	a.mux.HandleFunc("POST /api/auctions", a.require("manager")(a.handleCreateAuction))
	a.mux.HandleFunc("GET /api/auctions", a.auth(a.handleListAuctions))
	a.mux.HandleFunc("GET /api/auctions/{id}", a.auth(a.handleGetAuction))
	a.mux.HandleFunc("POST /api/auctions/{id}/bid", a.require("wholesaler")(a.handleBid))
	a.mux.HandleFunc("POST /api/auctions/{id}/buy-now", a.require("wholesaler")(a.handleBuyNow))
	a.mux.HandleFunc("POST /api/auctions/{id}/close", a.require("manager")(a.handleCloseAuction))

	// 批发商库存 / 调价 / 跨城调货
	a.mux.HandleFunc("GET /api/inventories", a.auth(a.handleListInventories))
	a.mux.HandleFunc("GET /api/market", a.auth(a.handleListMarket))
	a.mux.HandleFunc("POST /api/inventories/{id}/listing", a.require("wholesaler")(a.handleSetListing))
	a.mux.HandleFunc("POST /api/inventories/{id}/price", a.require("wholesaler")(a.handleChangePrice))
	a.mux.HandleFunc("GET /api/price-changes", a.require("wholesaler", "finance", "manager")(a.handleListPriceChanges))
	a.mux.HandleFunc("POST /api/transfers", a.require("wholesaler")(a.handleCreateTransfer))
	a.mux.HandleFunc("GET /api/transfers", a.auth(a.handleListTransfers))

	// 花店采购订单
	a.mux.HandleFunc("POST /api/orders", a.require("shop")(a.handleCreateOrder))
	a.mux.HandleFunc("GET /api/orders", a.auth(a.handleListOrders))
	a.mux.HandleFunc("GET /api/orders/{id}", a.auth(a.handleGetOrder))
	a.mux.HandleFunc("POST /api/orders/{id}/pay", a.require("shop", "finance")(a.handlePayOrder))
	a.mux.HandleFunc("POST /api/orders/{id}/cancel", a.require("shop")(a.handleCancelOrder))
	a.mux.HandleFunc("POST /api/orders/{id}/split", a.require("shop")(a.handleSplitOrder))

	// 配送
	a.mux.HandleFunc("POST /api/deliveries", a.require("logistics", "manager")(a.handleDispatch))
	a.mux.HandleFunc("GET /api/deliveries", a.auth(a.handleListDeliveries))
	a.mux.HandleFunc("POST /api/deliveries/{id}/status", a.require("logistics", "manager")(a.handleDeliveryStatus))

	// 退货 / 花农信用
	a.mux.HandleFunc("POST /api/orders/{id}/returns", a.require("shop")(a.handleCreateReturn))
	a.mux.HandleFunc("GET /api/returns", a.auth(a.handleListReturns))
	a.mux.HandleFunc("POST /api/returns/{id}/review", a.require("qc")(a.handleReviewReturn))
	a.mux.HandleFunc("GET /api/returns/reconciliation", a.require("finance", "wholesaler", "manager")(a.handleReturnReconciliation))

	// 财务：账期 / 结算
	a.mux.HandleFunc("GET /api/receivables", a.require("finance", "manager")(a.handleListReceivables))
	a.mux.HandleFunc("POST /api/receivables/{id}/settle", a.require("finance")(a.handleSettleReceivable))
	a.mux.HandleFunc("POST /api/settlements/generate", a.require("finance", "manager")(a.handleGenerateSettlement))
	a.mux.HandleFunc("GET /api/settlements", a.require("finance", "manager", "wholesaler", "farmer", "shop")(a.handleListSettlements))
	a.mux.HandleFunc("GET /api/settlements/{id}", a.require("finance", "manager", "wholesaler", "farmer", "shop")(a.handleGetSettlement))

	// 市场看板
	a.mux.HandleFunc("GET /api/analytics/overview", a.auth(a.handleAnalyticsOverview))
	a.mux.HandleFunc("GET /api/analytics/stability", a.auth(a.handleAnalyticsStability))
	a.mux.HandleFunc("GET /api/farmers/ranking", a.auth(a.handleFarmerRanking))

	// 冷库库位不足调度（凌晨到货高峰爆满）
	a.mux.HandleFunc("GET /api/coldchain/board", a.auth(a.handleColdBoard))
	a.mux.HandleFunc("GET /api/coldchain/sensitivity", a.auth(a.handleListSensitivity))
	a.mux.HandleFunc("PUT /api/coldchain/sensitivity/{category}", a.require("manager")(a.handleUpsertSensitivity))
	a.mux.HandleFunc("GET /api/coldchain/queue", a.auth(a.handleColdQueue))
	a.mux.HandleFunc("POST /api/coldchain/queue", a.require("coldstore", "manager", "farmer", "broker")(a.handleEnqueue))
	a.mux.HandleFunc("POST /api/coldchain/queue/{id}/admit", a.require("coldstore", "manager")(a.handleAdmit))
	a.mux.HandleFunc("POST /api/coldchain/queue/{id}/release", a.require("coldstore", "manager", "farmer", "broker")(a.handleReleaseQueue))
	a.mux.HandleFunc("GET /api/coldchain/incidents", a.auth(a.handleListIncidents))
	a.mux.HandleFunc("POST /api/coldchain/incidents", a.require("coldstore", "manager", "qc")(a.handleCreateIncident))
	a.mux.HandleFunc("POST /api/coldchain/incidents/{id}/confirm", a.require("coldstore", "wholesaler", "manager")(a.handleConfirmIncident))
	a.mux.HandleFunc("POST /api/coldchain/incidents/{id}/review", a.require("coldstore", "manager")(a.handleReviewIncident))
	a.mux.HandleFunc("POST /api/coldchain/incidents/{id}/adjustment", a.require("manager")(a.handleCreateAdjustment))
	a.mux.HandleFunc("GET /api/coldchain/adjustments", a.auth(a.handleListAdjustments))
	a.mux.HandleFunc("POST /api/coldchain/adjustments/{id}/confirm", a.require("broker", "manager")(a.handleBrokerConfirmAdjustment))

	// 站内通知（入场调整通知经纪人等）
	a.mux.HandleFunc("GET /api/notifications", a.auth(a.handleListNotifications))
	a.mux.HandleFunc("POST /api/notifications/{id}/read", a.auth(a.handleReadNotification))
	a.mux.HandleFunc("POST /api/notifications/read-all", a.auth(a.handleReadAllNotifications))
}

func (a *app) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := a.db.PingContext(r.Context()); err != nil {
		fail(w, http.StatusServiceUnavailable, "db: "+err.Error())
		return
	}
	if err := a.rdb.Ping(r.Context()).Err(); err != nil {
		fail(w, http.StatusServiceUnavailable, "redis: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now()})
}
