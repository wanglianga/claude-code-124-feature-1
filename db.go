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

-- 配送出库时的分拣照片（短枝复核的第 1 组证据之一）
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS sorting_photos TEXT NOT NULL DEFAULT '';

-- 短枝损耗复核单：花店反馈短枝比例高于竞价说明 → 服务汇聚四组证据 → 质检确认 → 账扣/争议
CREATE TABLE IF NOT EXISTS short_stem_reviews (
  id SERIAL PRIMARY KEY,
  review_no TEXT UNIQUE,
  order_id INT NOT NULL REFERENCES orders(id),
  batch_id INT NOT NULL REFERENCES flower_batches(id),
  shop_id INT NOT NULL REFERENCES users(id),
  wholesaler_id INT NOT NULL REFERENCES users(id),
  farmer_id INT NOT NULL REFERENCES users(id),
  bundles INT NOT NULL,                       -- 涉及扎数
  auction_declared_short_ratio NUMERIC(4,3) NOT NULL,  -- 竞价说明短枝比例（取自分拣质检）
  qc_grade_snapshot TEXT NOT NULL DEFAULT '',          -- 证据②质检等级
  outbound_bundles INT NOT NULL DEFAULT 0,             -- 证据③出库扎数
  sorting_photos TEXT NOT NULL DEFAULT '',             -- 证据①分拣照片（出库拍摄，URL 逗号分隔）
  outbound_photo TEXT NOT NULL DEFAULT '',             -- 证据③出库照片
  actual_short_ratio NUMERIC(4,3) NOT NULL,            -- 证据④到店复拍实测短枝比例
  reshoot_photos TEXT NOT NULL DEFAULT '',             -- 证据④到店复拍照片（URL 逗号分隔）
  shop_remark TEXT NOT NULL DEFAULT '',
  -- pending_qc 待质检 / confirmed 已质检确认 / charged 花农已确认账扣 /
  -- disputed 花农异议冻结中 / closed_upheld 争议关闭维持原判 / closed_overturned 争议关闭推翻
  status TEXT NOT NULL DEFAULT 'pending_qc',
  resolution TEXT NOT NULL DEFAULT '',        -- price_diff 差价补偿 / return 退货 / next_discount 下一批折扣
  qc_note TEXT NOT NULL DEFAULT '',
  confirmed_qc_by INT REFERENCES users(id),
  confirmed_qc_at TIMESTAMPTZ,
  -- 三种处理结果的金额/数量（质检确认时落定）
  price_diff_per_bundle NUMERIC(10,2) NOT NULL DEFAULT 0,
  price_diff_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
  return_bundles INT NOT NULL DEFAULT 0,
  return_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
  next_discount_rate NUMERIC(5,4) NOT NULL DEFAULT 0,
  commitment_id INT,                           -- 生成的下一批折扣承诺
  frozen_amount NUMERIC(14,2) NOT NULL DEFAULT 0,  -- 争议期间财务冻结的差价
  -- 花农账扣确认
  farmer_responsibility TEXT NOT NULL DEFAULT 'pending',  -- pending/yes/no（账扣确认/异议）
  farmer_charged BOOLEAN NOT NULL DEFAULT false,
  confirmed_by INT REFERENCES users(id),
  confirmed_at TIMESTAMPTZ,
  farmer_seen_at TIMESTAMPTZ,
  -- 争议
  dispute_raised_at TIMESTAMPTZ,
  dispute_reason TEXT NOT NULL DEFAULT '',
  closed_by INT REFERENCES users(id),
  closed_at TIMESTAMPTZ,
  close_note TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 花农下一批折扣承诺（质检确认生成，结拍时自动核销冲减花农成交收入）
CREATE TABLE IF NOT EXISTS farmer_discount_commitments (
  id SERIAL PRIMARY KEY,
  farmer_id INT NOT NULL REFERENCES users(id),
  review_id INT NOT NULL REFERENCES short_stem_reviews(id),
  rate NUMERIC(5,4) NOT NULL,                 -- 折扣率，如 0.10 = 下批成交价让利 10%
  status TEXT NOT NULL DEFAULT 'pending',     -- pending 待核销 / redeemed 已核销 / voided 争议推翻作废
  redeemed_auction_id INT REFERENCES auctions(id),
  redeemed_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  redeemed_at TIMESTAMPTZ
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
CREATE INDEX IF NOT EXISTS idx_ssr_farmer ON short_stem_reviews(farmer_id, status);
CREATE INDEX IF NOT EXISTS idx_ssr_order ON short_stem_reviews(order_id);
CREATE INDEX IF NOT EXISTS idx_ssr_status ON short_stem_reviews(status);
CREATE INDEX IF NOT EXISTS idx_commitment_farmer ON farmer_discount_commitments(farmer_id, status);
`
