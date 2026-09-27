package main

import (
	"context"
	"database/sql"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

func connectDB(url string) *sql.DB {
	var db *sql.DB
	var err error
	for i := 0; i < 60; i++ {
		db, err = sql.Open("pgx", url)
		if err == nil {
			err = db.Ping()
		}
		if err == nil {
			log.Println("PostgreSQL 已连接")
			return db
		}
		log.Printf("等待 PostgreSQL 就绪(%d/60): %v", i+1, err)
		time.Sleep(time.Second)
	}
	log.Fatalf("PostgreSQL 连接失败: %v", err)
	return nil
}

func connectRedis(addr string) *redis.Client {
	c := redis.NewClient(&redis.Options{Addr: addr})
	for i := 0; i < 60; i++ {
		if err := c.Ping(context.Background()).Err(); err == nil {
			log.Println("Redis 已连接")
			return c
		}
		log.Printf("等待 Redis 就绪(%d/60)", i+1)
		time.Sleep(time.Second)
	}
	log.Fatalf("Redis 连接失败: %s", addr)
	return nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(schemaSQL)
	return err
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS users (
  id SERIAL PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL,             -- farmer/broker/qc/wholesaler/shop/coldstore/logistics/finance/manager
  name TEXT NOT NULL,
  credit_limit NUMERIC(14,2) NOT NULL DEFAULT 0,   -- 账期额度（花店/婚庆）
  credit_days INT NOT NULL DEFAULT 0,              -- 账期天数
  credit_score INT NOT NULL DEFAULT 100,           -- 花农信用分（影响竞价排序）
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS storage_slots (
  id SERIAL PRIMARY KEY,
  code TEXT UNIQUE NOT NULL,
  zone TEXT NOT NULL DEFAULT 'A',
  capacity_bundles INT NOT NULL,
  occupied_bundles INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS flower_batches (
  id SERIAL PRIMARY KEY,
  batch_no TEXT UNIQUE,
  farmer_id INT NOT NULL REFERENCES users(id),
  broker_id INT REFERENCES users(id),
  category TEXT NOT NULL,         -- rose/lily/tulip/eustoma/eucalyptus/holiday_mix
  origin TEXT NOT NULL,           -- 产地
  cut_at TIMESTAMPTZ NOT NULL,    -- 采切时间
  stem_length_cm NUMERIC(5,1) NOT NULL,  -- 枝长
  openness NUMERIC(3,1) NOT NULL,        -- 开放度 1.0-5.0
  bundles INT NOT NULL,                  -- 扎数
  stems_per_bundle INT NOT NULL DEFAULT 20,
  arrival_temp_c NUMERIC(4,1) NOT NULL,  -- 冷链到货温度
  sellable_hours INT NOT NULL,           -- 可售时长（小时）
  grade TEXT NOT NULL DEFAULT '',        -- 质检等级 A/B/C/D
  risk_flags TEXT NOT NULL DEFAULT '',   -- 风险标记，逗号分隔
  status TEXT NOT NULL DEFAULT 'qc_pending',
  -- qc_pending/qc_passed/qc_flagged/stored/in_auction/sold/expired/closed
  storage_slot_id INT REFERENCES storage_slots(id),
  stored_at TIMESTAMPTZ,
  out_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS qc_records (
  id SERIAL PRIMARY KEY,
  batch_id INT NOT NULL REFERENCES flower_batches(id),
  inspector_id INT NOT NULL REFERENCES users(id),
  grade TEXT NOT NULL,
  openness_evenness NUMERIC(4,3) NOT NULL,  -- 开放度均匀度 0-1
  short_stem_ratio NUMERIC(4,3) NOT NULL,   -- 短枝比例 0-1
  temp_ok BOOLEAN NOT NULL DEFAULT true,
  notes TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS preservative_batches (
  id SERIAL PRIMARY KEY,
  code TEXT UNIQUE NOT NULL,
  brand TEXT NOT NULL,
  ingredient TEXT NOT NULL DEFAULT '',
  expires_at DATE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS batch_preservatives (
  id SERIAL PRIMARY KEY,
  batch_id INT NOT NULL REFERENCES flower_batches(id),
  preservative_id INT NOT NULL REFERENCES preservative_batches(id),
  dosage_ml NUMERIC(8,1) NOT NULL,
  applied_by INT NOT NULL REFERENCES users(id),
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS auctions (
  id SERIAL PRIMARY KEY,
  batch_id INT NOT NULL REFERENCES flower_batches(id),
  mode TEXT NOT NULL,             -- auction 竞价 / fixed 一口价
  start_price NUMERIC(10,2) NOT NULL,   -- 每扎起拍价
  buy_now_price NUMERIC(10,2),          -- 一口价
  min_increment NUMERIC(10,2) NOT NULL DEFAULT 1,
  current_price NUMERIC(10,2),
  winner_id INT REFERENCES users(id),
  status TEXT NOT NULL DEFAULT 'active',  -- active/closed/unsold/cancelled
  starts_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  ends_at TIMESTAMPTZ NOT NULL,
  created_by INT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bids (
  id SERIAL PRIMARY KEY,
  auction_id INT NOT NULL REFERENCES auctions(id),
  wholesaler_id INT NOT NULL REFERENCES users(id),
  price NUMERIC(10,2) NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS inventories (
  id SERIAL PRIMARY KEY,
  wholesaler_id INT NOT NULL REFERENCES users(id),
  batch_id INT NOT NULL REFERENCES flower_batches(id),
  auction_id INT REFERENCES auctions(id),
  bundles_total INT NOT NULL,
  bundles_available INT NOT NULL,
  bundles_reserved INT NOT NULL DEFAULT 0,  -- 被未付款订单锁定
  cost_price NUMERIC(10,2) NOT NULL,        -- 每扎成本（成交价）
  list_price NUMERIC(10,2),                 -- 每扎挂牌价（可改价）
  listed BOOLEAN NOT NULL DEFAULT false,
  status TEXT NOT NULL DEFAULT 'in_stock',  -- in_stock/sold_out/transferred_out
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS price_changes (
  id SERIAL PRIMARY KEY,
  inventory_id INT NOT NULL REFERENCES inventories(id),
  old_price NUMERIC(10,2),
  new_price NUMERIC(10,2) NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  changed_by INT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders (
  id SERIAL PRIMARY KEY,
  order_no TEXT UNIQUE,
  shop_id INT NOT NULL REFERENCES users(id),
  inventory_id INT NOT NULL REFERENCES inventories(id),
  wholesaler_id INT NOT NULL REFERENCES users(id),
  batch_id INT NOT NULL REFERENCES flower_batches(id),
  bundles INT NOT NULL,
  price_per_bundle NUMERIC(10,2) NOT NULL,  -- 下单时锁定价格，批发商改价不影响
  total_amount NUMERIC(14,2) NOT NULL,
  purpose TEXT NOT NULL,          -- wedding/retail/livestream/enterprise
  payment_type TEXT NOT NULL,     -- immediate/credit(账期)
  status TEXT NOT NULL DEFAULT 'pending_payment',
  -- pending_payment/paid/dispatched/delivered/completed/cancelled/timeout_cancelled
  is_presale BOOLEAN NOT NULL DEFAULT false,  -- 节日预售锁货
  festival TEXT NOT NULL DEFAULT '',
  parent_order_id INT REFERENCES orders(id),  -- 节日订单拆分来源
  note TEXT NOT NULL DEFAULT '',
  payment_deadline TIMESTAMPTZ NOT NULL,
  paid_at TIMESTAMPTZ,
  delivered_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS payments (
  id SERIAL PRIMARY KEY,
  order_id INT NOT NULL REFERENCES orders(id),
  amount NUMERIC(14,2) NOT NULL,
  method TEXT NOT NULL DEFAULT 'online',  -- online/credit/offline
  recorded_by INT REFERENCES users(id),
  paid_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS credit_receivables (
  id SERIAL PRIMARY KEY,
  order_id INT NOT NULL REFERENCES orders(id),
  customer_id INT NOT NULL REFERENCES users(id),
  amount NUMERIC(14,2) NOT NULL,
  due_date TIMESTAMPTZ NOT NULL,
  status TEXT NOT NULL DEFAULT 'open',   -- open/overdue/settled
  settled_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS transfers (
  id SERIAL PRIMARY KEY,
  inventory_id INT NOT NULL REFERENCES inventories(id),
  wholesaler_id INT NOT NULL REFERENCES users(id),
  bundles INT NOT NULL,
  to_city TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',  -- pending/in_transit/arrived
  created_by INT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS deliveries (
  id SERIAL PRIMARY KEY,
  order_id INT REFERENCES orders(id),
  transfer_id INT REFERENCES transfers(id),
  dispatcher_id INT REFERENCES users(id),
  to_city TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',  -- pending/in_transit/delivered
  departed_at TIMESTAMPTZ,
  delivered_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS returns (
  id SERIAL PRIMARY KEY,
  order_id INT NOT NULL REFERENCES orders(id),
  batch_id INT NOT NULL REFERENCES flower_batches(id),
  shop_id INT NOT NULL REFERENCES users(id),
  bundles INT NOT NULL,
  reason TEXT NOT NULL,          -- petal_drop/quality/temp_damage/short_stem/other
  loss_ratio NUMERIC(5,3) NOT NULL,  -- 损耗比例 0-1
  claim_amount NUMERIC(14,2) NOT NULL,
  status TEXT NOT NULL DEFAULT 'requested',  -- requested/approved/rejected
  reviewed_by INT REFERENCES users(id),
  reviewed_at TIMESTAMPTZ,
  farmer_responsible BOOLEAN NOT NULL DEFAULT false,  -- 判定花农责任 -> 扣信用
  review_note TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS farmer_credit_events (
  id SERIAL PRIMARY KEY,
  farmer_id INT NOT NULL REFERENCES users(id),
  delta INT NOT NULL,
  reason TEXT NOT NULL,
  return_id INT REFERENCES returns(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== 冷库库位不足调度（凌晨到货高峰爆满场景） =====

-- 花材批次补充：敏感度等级（高/中/低，决定爆满时优先入库）+ 预计提货时间
ALTER TABLE flower_batches ADD COLUMN IF NOT EXISTS sensitivity TEXT NOT NULL DEFAULT 'medium';
ALTER TABLE flower_batches ADD COLUMN IF NOT EXISTS expected_pickup_at TIMESTAMPTZ;

-- 竞价/一口价场次补充：预计提货时间（成交后回写到批次，作为优先入库依据之一）
ALTER TABLE auctions ADD COLUMN IF NOT EXISTS estimated_pickup_at TIMESTAMPTZ;

-- 临时保温记录：冷库爆满时未入库批次的保温方式与损耗责任
CREATE TABLE IF NOT EXISTS holding_records (
  id SERIAL PRIMARY KEY,
  batch_id INT NOT NULL REFERENCES flower_batches(id),
  method TEXT NOT NULL,              -- ice_box冰袋保温箱/refrigerated_truck冷藏车/insulated_cover保温被/cool_room临租小冷库/other
  responsibility TEXT NOT NULL,      -- 保温期间损耗责任承担方：coldstore冷库/wholesaler批发商/farmer花农/broker经纪人
  note TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active',  -- active保温中/stored已入库/failed保温失败
  failure_note TEXT NOT NULL DEFAULT '',  -- 保温失败说明（开放过快）
  failed_at TIMESTAMPTZ,
  -- 保温失败造成开放过快时，冷库负责人与批发商双方确认责任（双签）
  coldstore_confirmed_by INT REFERENCES users(id),
  coldstore_confirmed_at TIMESTAMPTZ,
  coldstore_opinion TEXT NOT NULL DEFAULT '',     -- 冷库方认定的责任方
  wholesaler_confirmed_by INT REFERENCES users(id),
  wholesaler_confirmed_at TIMESTAMPTZ,
  wholesaler_opinion TEXT NOT NULL DEFAULT '',    -- 批发方认定的责任方
  final_responsibility TEXT NOT NULL DEFAULT '',  -- 双方一致后的最终责任方（disputed=存在分歧）
  compensation NUMERIC(14,2) NOT NULL DEFAULT 0,  -- 确认后计入结算的损耗赔付（责任方为批发商时记其应付）
  created_by INT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 冷库端复盘：对保温失败/爆满事件的同步复盘记录
CREATE TABLE IF NOT EXISTS cold_store_reviews (
  id SERIAL PRIMARY KEY,
  holding_id INT NOT NULL REFERENCES holding_records(id),
  reviewer_id INT NOT NULL REFERENCES users(id),
  root_cause TEXT NOT NULL DEFAULT '',   -- 根因分析
  improvement TEXT NOT NULL DEFAULT '',  -- 改进措施
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 入场顺序调整：市场针对某到货时段调整入场顺序，冷库复盘后通知经纪人确认
CREATE TABLE IF NOT EXISTS entry_adjustments (
  id SERIAL PRIMARY KEY,
  time_slot TEXT NOT NULL,           -- 到货时段，如 02:00-04:00
  holding_id INT REFERENCES holding_records(id),   -- 关联的保温失败记录（可空）
  reason TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',   -- 调整后的入场顺序说明
  status TEXT NOT NULL DEFAULT 'pending',          -- pending待经纪人确认/confirmed已确认
  created_by INT NOT NULL REFERENCES users(id),
  broker_id INT REFERENCES users(id),              -- 需确认的经纪人（空=全体经纪人）
  confirmed_by INT REFERENCES users(id),
  confirmed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 站内通知（经纪人确认入场调整等）
CREATE TABLE IF NOT EXISTS notifications (
  id SERIAL PRIMARY KEY,
  user_id INT REFERENCES users(id),  -- NULL=广播给该角色全体
  role TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,                -- entry_adjustment入场调整/holding_failed保温失败/holding_timeout保温超时
  ref_id INT NOT NULL DEFAULT 0,
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  read_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS settlements (
  id SERIAL PRIMARY KEY,
  month TEXT NOT NULL,          -- YYYY-MM
  entity_role TEXT NOT NULL,    -- farmer/wholesaler/shop
  entity_id INT NOT NULL REFERENCES users(id),
  gross_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
  loss_deduction NUMERIC(14,2) NOT NULL DEFAULT 0,   -- 损耗/退货扣款
  storage_fee NUMERIC(14,2) NOT NULL DEFAULT 0,      -- 冷库费
  credit_deduction NUMERIC(14,2) NOT NULL DEFAULT 0, -- 账期扣款/罚息
  net_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'generated',
  generated_by INT REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (month, entity_role, entity_id)
);

CREATE TABLE IF NOT EXISTS settlement_lines (
  id SERIAL PRIMARY KEY,
  settlement_id INT NOT NULL REFERENCES settlements(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  ref_type TEXT NOT NULL DEFAULT '',
  ref_id INT NOT NULL DEFAULT 0,
  description TEXT NOT NULL DEFAULT '',
  amount NUMERIC(14,2) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_batches_farmer ON flower_batches(farmer_id);
CREATE INDEX IF NOT EXISTS idx_batches_status ON flower_batches(status);
CREATE INDEX IF NOT EXISTS idx_auctions_status_ends ON auctions(status, ends_at);
CREATE INDEX IF NOT EXISTS idx_bids_auction ON bids(auction_id, price DESC);
CREATE INDEX IF NOT EXISTS idx_inventories_wholesaler ON inventories(wholesaler_id);
CREATE INDEX IF NOT EXISTS idx_orders_shop ON orders(shop_id);
CREATE INDEX IF NOT EXISTS idx_orders_batch ON orders(batch_id);
CREATE INDEX IF NOT EXISTS idx_orders_status_deadline ON orders(status, payment_deadline);
CREATE INDEX IF NOT EXISTS idx_returns_order ON returns(order_id);
CREATE INDEX IF NOT EXISTS idx_receivables_customer ON credit_receivables(customer_id, status);
CREATE INDEX IF NOT EXISTS idx_holding_batch ON holding_records(batch_id, status);
CREATE INDEX IF NOT EXISTS idx_notifications_user ON notifications(user_id, read_at);
CREATE INDEX IF NOT EXISTS idx_entry_adjustments_status ON entry_adjustments(status);
`
