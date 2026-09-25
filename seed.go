package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// seedDemo 写入演示数据（仅当 users 表为空时执行，幂等）
func (a *app) seedDemo() {
	var n int
	if err := a.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil || n > 0 {
		return
	}
	log.Println("写入演示数据…")
	ctx := context.Background()
	now := time.Now()

	tx, err := a.db.Begin()
	if err != nil {
		log.Printf("演示数据写入失败: %v", err)
		return
	}
	defer tx.Rollback()

	hash, _ := bcrypt.GenerateFromPassword([]byte("flower123"), 10)
	uid := map[string]int{}
	addUser := func(username, role, name string, creditLimit float64, creditDays int) {
		var id int
		if err := tx.QueryRow(
			`INSERT INTO users (username, password_hash, role, name, credit_limit, credit_days)
			 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			username, string(hash), role, name, creditLimit, creditDays).Scan(&id); err != nil {
			log.Printf("seed 用户失败 %s: %v", username, err)
		}
		uid[username] = id
	}
	addUser("farmer01", "farmer", "花农·昆明老张", 0, 0)
	addUser("farmer02", "farmer", "花农·凌源小李", 0, 0)
	addUser("broker01", "broker", "经纪人·王姐", 0, 0)
	addUser("qc01", "qc", "质检员·陈工", 0, 0)
	addUser("wholesaler01", "wholesaler", "批发商·恒信花卉", 0, 0)
	addUser("wholesaler02", "wholesaler", "批发商·馨源贸易", 0, 0)
	addUser("shop01", "shop", "花店·城南花坊", 0, 0)
	addUser("shop02", "shop", "婚庆·喜结良缘(账期)", 50000, 30)
	addUser("shop03", "shop", "花店·直播间小艾", 0, 0)
	addUser("cold01", "coldstore", "冷库·赵班长", 0, 0)
	addUser("ship01", "logistics", "配送·孙师傅", 0, 0)
	addUser("fin01", "finance", "财务·刘会计", 0, 0)
	addUser("boss01", "manager", "市场管理·吴经理", 0, 0)

	slotID := map[string]int{}
	addSlot := func(code, zone string, cap, occ int) {
		var id int
		if err := tx.QueryRow(
			`INSERT INTO storage_slots (code, zone, capacity_bundles, occupied_bundles) VALUES ($1,$2,$3,$4) RETURNING id`,
			code, zone, cap, occ).Scan(&id); err != nil {
			log.Printf("seed 库位失败: %v", err)
		}
		slotID[code] = id
	}
	addSlot("A-01", "A", 300, 80)
	addSlot("A-02", "A", 300, 60)
	addSlot("A-03", "A", 300, 85)
	addSlot("A-04", "A", 300, 0)
	addSlot("B-01", "B", 150, 40)
	addSlot("B-02", "B", 150, 0)

	var pres1, pres2 int
	tx.QueryRow(`INSERT INTO preservative_batches (code, brand, ingredient, expires_at)
		VALUES ('PB-2601','可利鲜','蔗糖+杀菌剂（玫瑰专用）',$1) RETURNING id`, now.AddDate(0, 6, 0)).Scan(&pres1)
	tx.QueryRow(`INSERT INTO preservative_batches (code, brand, ingredient, expires_at)
		VALUES ('PB-2602','花之寿','有机酸+乙烯抑制剂（百合/郁金香）',$1) RETURNING id`, now.AddDate(0, 4, 0)).Scan(&pres2)

	addBatch := func(farmer string, cat, origin string, cutAt time.Time, stem, open float64,
		bundles, spb int, temp float64, sellH int, grade, flags, status string,
		slotCode string, storedAt *time.Time) int {
		var id int
		var slot *int
		if slotCode != "" {
			if sid, ok := slotID[slotCode]; ok {
				slot = &sid
			}
		}
		if err := tx.QueryRow(
			`INSERT INTO flower_batches (farmer_id, category, origin, cut_at, stem_length_cm, openness,
			  bundles, stems_per_bundle, arrival_temp_c, sellable_hours, grade, risk_flags, status,
			  storage_slot_id, stored_at, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
			uid[farmer], cat, origin, cutAt, stem, open, bundles, spb, temp, sellH,
			grade, flags, status, slot, storedAt, cutAt.Add(2*time.Hour)).Scan(&id); err != nil {
			log.Printf("seed 批次失败: %v", err)
			return 0
		}
		tx.Exec(`UPDATE flower_batches SET batch_no=$1 WHERE id=$2`, fmt.Sprintf("B%06d", id), id)
		return id
	}
	addQC := func(batchID int, grade string, even, short float64, tempOK bool, notes string, at time.Time) {
		tx.Exec(`INSERT INTO qc_records (batch_id, inspector_id, grade, openness_evenness, short_stem_ratio, temp_ok, notes, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, batchID, uid["qc01"], grade, even, short, tempOK, notes, at)
	}
	h := time.Hour
	m := time.Minute

	// 批次1：玫瑰·昆明 —— 质检A，已入库，正在竞价（35 分钟后结拍）
	t1 := now.Add(-6 * h)
	b1 := addBatch("farmer01", "rose", "云南昆明", now.Add(-10*h), 70, 3.0, 80, 20, 4.5, 48, "A", "", "in_auction", "A-01", &t1)
	addQC(b1, "A", 0.92, 0.05, true, "花头整齐，枝杆挺拔", now.Add(-5*h))
	var auc1 int
	tx.QueryRow(`INSERT INTO auctions (batch_id, mode, start_price, min_increment, current_price, status, starts_at, ends_at, created_by)
		VALUES ($1,'auction',38,1,41,'active',$2,$3,$4) RETURNING id`,
		b1, now.Add(-25*m), now.Add(35*m), uid["boss01"]).Scan(&auc1)
	tx.Exec(`INSERT INTO bids (auction_id, wholesaler_id, price, created_at) VALUES ($1,$2,39,$3)`, auc1, uid["wholesaler01"], now.Add(-20*m))
	tx.Exec(`INSERT INTO bids (auction_id, wholesaler_id, price, created_at) VALUES ($1,$2,41,$3)`, auc1, uid["wholesaler02"], now.Add(-8*m))

	// 批次2：百合·凌源 —— 质检B，已入库待上拍
	t2 := now.Add(-5 * h)
	b2 := addBatch("farmer02", "lily", "辽宁凌源", now.Add(-9*h), 65, 2.5, 60, 10, 5.0, 60, "B", "", "stored", "A-02", &t2)
	addQC(b2, "B", 0.85, 0.08, true, "个别花苞偏生", now.Add(-4*h))

	// 批次3：郁金香·昆明 —— 刚到货待质检
	addBatch("farmer01", "tulip", "云南昆明", now.Add(-3*h), 45, 2.0, 50, 10, 6.0, 36, "", "", "qc_pending", "", nil)

	// 批次4：洋桔梗·广州 —— 质检D 打回（开放度不均+短枝比例高）
	b4 := addBatch("farmer02", "eustoma", "广东广州", now.Add(-8*h), 40, 3.5, 40, 10, 9.5, 30, "D",
		"uneven_openness,high_short_stem,temp_abnormal", "qc_flagged", "", nil)
	addQC(b4, "D", 0.55, 0.32, false, "开放度不均，短枝比例 32%，冷链温度偏高", now.Add(-3*h))

	// 批次5：节日混搭·昆明 —— 批发商01 拍得 100 扎@40，部分已售/退货/调货
	t5 := now.Add(-30 * h)
	b5 := addBatch("farmer01", "holiday_mix", "云南昆明", now.Add(-34*h), 60, 3.0, 100, 15, 5.5, 72, "A", "", "sold", "A-03", &t5)
	addQC(b5, "A", 0.9, 0.06, true, "混搭比例均匀", now.Add(-29*h))
	var auc5 int
	tx.QueryRow(`INSERT INTO auctions (batch_id, mode, start_price, min_increment, current_price, winner_id, status, starts_at, ends_at, created_by)
		VALUES ($1,'auction',35,1,40,$2,'closed',$3,$4,$5) RETURNING id`,
		b5, uid["wholesaler01"], now.Add(-29*h), now.Add(-28*h), uid["boss01"]).Scan(&auc5)
	tx.Exec(`INSERT INTO bids (auction_id, wholesaler_id, price, created_at) VALUES ($1,$2,40,$3)`, auc5, uid["wholesaler01"], now.Add(-28*h))
	var inv5 int
	tx.QueryRow(`INSERT INTO inventories (wholesaler_id, batch_id, auction_id, bundles_total, bundles_available,
		bundles_reserved, cost_price, list_price, listed, created_at)
		VALUES ($1,$2,$3,100,60,5,40,52,true,$4) RETURNING id`,
		uid["wholesaler01"], b5, auc5, now.Add(-28*h)).Scan(&inv5)
	tx.Exec(`INSERT INTO price_changes (inventory_id, old_price, new_price, reason, changed_by, created_at)
		VALUES ($1,48,52,'节前需求上涨，临时调价',$2,$3)`, inv5, uid["wholesaler01"], now.Add(-12*h))

	addOrder := func(shop string, invID, bundles int, price float64, purpose, payType, status string,
		presale bool, festival string, deadline time.Time, paidAt, deliveredAt *time.Time, createdAt time.Time) int {
		var id int
		if err := tx.QueryRow(
			`INSERT INTO orders (shop_id, inventory_id, wholesaler_id, batch_id, bundles, price_per_bundle,
			  total_amount, purpose, payment_type, status, is_presale, festival, payment_deadline, paid_at, delivered_at, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
			uid[shop], invID, uid["wholesaler01"], b5, bundles, price, float64(bundles)*price,
			purpose, payType, status, presale, festival, deadline, paidAt, deliveredAt, createdAt).Scan(&id); err != nil {
			log.Printf("seed 订单失败: %v", err)
			return 0
		}
		tx.Exec(`UPDATE orders SET order_no=$1 WHERE id=$2`, fmt.Sprintf("O%08d", id), id)
		return id
	}
	// 订单1：shop01 零售 10 扎，已送达 → 掉瓣退货（已裁定）
	p1 := now.Add(-27 * h)
	d1 := now.Add(-20 * h)
	o1 := addOrder("shop01", inv5, 10, 52, "retail", "immediate", "delivered", false, "",
		now.Add(-26*h), &p1, &d1, now.Add(-27*h))
	tx.Exec(`INSERT INTO payments (order_id, amount, method, recorded_by, paid_at) VALUES ($1,520,'online',$2,$3)`,
		o1, uid["shop01"], p1)
	tx.Exec(`INSERT INTO deliveries (order_id, dispatcher_id, to_city, status, departed_at, delivered_at, created_at)
		VALUES ($1,$2,'本市','delivered',$3,$4,$5)`, o1, uid["ship01"], now.Add(-26*h), d1, now.Add(-26*h))
	var ret1 int
	tx.QueryRow(`INSERT INTO returns (order_id, batch_id, shop_id, bundles, reason, loss_ratio, claim_amount,
		status, reviewed_by, reviewed_at, farmer_responsible, review_note, created_at)
		VALUES ($1,$2,$3,2,'petal_drop',0.3,31.2,'approved',$4,$5,true,'到店掉瓣属实，采切成熟度偏高',$6) RETURNING id`,
		o1, b5, uid["shop01"], uid["qc01"], now.Add(-18*h), now.Add(-19*h)).Scan(&ret1)
	tx.Exec(`INSERT INTO farmer_credit_events (farmer_id, delta, reason, return_id, created_at)
		VALUES ($1,-3,'退货判定花农责任（到店掉瓣）',$2,$3)`, uid["farmer01"], ret1, now.Add(-18*h))
	tx.Exec(`UPDATE users SET credit_score=97 WHERE id=$1`, uid["farmer01"])

	// 订单2：shop02 婚庆账期 20 扎，已逾期 1 小时
	p2 := now.Add(-26 * h)
	o2 := addOrder("shop02", inv5, 20, 52, "wedding", "credit", "paid", false, "",
		now.Add(-25*h), &p2, nil, now.Add(-26*h))
	tx.Exec(`INSERT INTO payments (order_id, amount, method, recorded_by, paid_at) VALUES ($1,1040,'credit',$2,$3)`,
		o2, uid["shop02"], p2)
	tx.Exec(`INSERT INTO credit_receivables (order_id, customer_id, amount, due_date, status, created_at)
		VALUES ($1,$2,1040,$3,'overdue',$4)`, o2, uid["shop02"], now.Add(-1*h), p2)

	// 订单3：shop03 直播花束 5 扎，待付款（25 分钟后超时）
	o3 := addOrder("shop03", inv5, 5, 52, "livestream", "immediate", "pending_payment", false, "",
		now.Add(25*m), nil, nil, now.Add(-5*m))
	_ = o3

	// 批次6：尤加利·昆明 —— 批发商02 拍得并上架，含一笔节日预售锁货
	t6 := now.Add(-9 * h)
	b6 := addBatch("farmer02", "eucalyptus", "云南昆明", now.Add(-13*h), 55, 2.0, 40, 10, 4.0, 96, "B", "", "sold", "B-01", &t6)
	addQC(b6, "B", 0.88, 0.1, true, "叶色正常", now.Add(-8*h))
	var auc6 int
	tx.QueryRow(`INSERT INTO auctions (batch_id, mode, start_price, buy_now_price, min_increment, current_price, winner_id, status, starts_at, ends_at, created_by)
		VALUES ($1,'fixed',15,15,1,15,$2,'closed',$3,$4,$5) RETURNING id`,
		b6, uid["wholesaler02"], now.Add(-9*h), now.Add(-8*h), uid["boss01"]).Scan(&auc6)
	tx.Exec(`INSERT INTO bids (auction_id, wholesaler_id, price, created_at) VALUES ($1,$2,15,$3)`, auc6, uid["wholesaler02"], now.Add(-8*h))
	var inv6 int
	tx.QueryRow(`INSERT INTO inventories (wholesaler_id, batch_id, auction_id, bundles_total, bundles_available,
		bundles_reserved, cost_price, list_price, listed, created_at)
		VALUES ($1,$2,$3,40,35,5,15,22,true,$4) RETURNING id`,
		uid["wholesaler02"], b6, auc6, now.Add(-8*h)).Scan(&inv6)
	// 节日预售锁货：shop02 为七夕预售 5 扎尤加利
	addOrderWithWholesaler := func(shop string, invID, wholesalerID, batchID, bundles int, price float64,
		purpose, payType, status string, presale bool, festival string, deadline time.Time, createdAt time.Time) {
		var id int
		if err := tx.QueryRow(
			`INSERT INTO orders (shop_id, inventory_id, wholesaler_id, batch_id, bundles, price_per_bundle,
			  total_amount, purpose, payment_type, status, is_presale, festival, payment_deadline, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
			uid[shop], invID, wholesalerID, batchID, bundles, price, float64(bundles)*price,
			purpose, payType, status, presale, festival, deadline, createdAt).Scan(&id); err != nil {
			log.Printf("seed 预售单失败: %v", err)
			return
		}
		tx.Exec(`UPDATE orders SET order_no=$1 WHERE id=$2`, fmt.Sprintf("O%08d", id), id)
	}
	addOrderWithWholesaler("shop02", inv6, uid["wholesaler02"], b6, 5, 22, "wedding", "immediate",
		"pending_payment", true, "七夕", now.Add(72*h), now.Add(-2*h))

	// 跨城调货：批发商01 调 5 扎节日混搭到上海，在途
	var tr1 int
	tx.QueryRow(`INSERT INTO transfers (inventory_id, wholesaler_id, bundles, to_city, status, created_by, created_at)
		VALUES ($1,$2,5,'上海','in_transit',$3,$4) RETURNING id`,
		inv5, uid["wholesaler01"], uid["wholesaler01"], now.Add(-2*h)).Scan(&tr1)
	tx.Exec(`INSERT INTO deliveries (transfer_id, dispatcher_id, to_city, status, departed_at, created_at)
		VALUES ($1,$2,'上海','in_transit',$3,$4)`, tr1, uid["ship01"], now.Add(-2*h), now.Add(-2*h))

	// 保鲜剂施用：批次1 玫瑰用可利鲜
	tx.Exec(`INSERT INTO batch_preservatives (batch_id, preservative_id, dosage_ml, applied_by, applied_at)
		VALUES ($1,$2,500,$3,$4)`, b1, pres1, uid["cold01"], now.Add(-5*h))
	tx.Exec(`UPDATE flower_batches SET sellable_hours=sellable_hours+12 WHERE id=$1`, b1)
	_ = pres2

	if err := tx.Commit(); err != nil {
		log.Printf("演示数据提交失败: %v", err)
		return
	}
	a.syncFarmerRanking(ctx)
	log.Println("演示数据就绪：13 个账号、6 个批次、1 场进行中的竞价、库存/订单/退货/账期/调货样例")
}
