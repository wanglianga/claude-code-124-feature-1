package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// ---------- 花店采购订单 ----------

// handleCreateOrder 花店下单锁货（婚礼/零售/直播/企业；支持节日预售锁货与账期）
func (a *app) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		InventoryID int    `json:"inventory_id"`
		Bundles     int    `json:"bundles"`
		Purpose     string `json:"purpose"`      // wedding/retail/livestream/enterprise
		PaymentType string `json:"payment_type"` // immediate/credit
		IsPresale   bool   `json:"is_presale"`   // 节日预售锁货
		Festival    string `json:"festival"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.InventoryID == 0 || in.Bundles <= 0 {
		fail(w, 400, "请指定库存与扎数")
		return
	}
	if !contains(purposes, in.Purpose) {
		fail(w, 400, "用途须为 wedding/retail/livestream/enterprise")
		return
	}
	if in.PaymentType != "immediate" && in.PaymentType != "credit" {
		fail(w, 400, "payment_type 须为 immediate 或 credit")
		return
	}
	if in.PaymentType == "credit" && u.CreditDays == 0 {
		fail(w, 409, "当前账号未开通账期，请选择立即支付")
		return
	}
	if in.IsPresale && in.Festival == "" {
		fail(w, 400, "节日预售锁货需填写 festival（如 七夕/情人节）")
		return
	}

	// Redis 互斥锁，防止同一库存并发超卖
	lockKey := fmt.Sprintf("lock:inv:%d", in.InventoryID)
	ok, err := a.rdb.SetNX(r.Context(), lockKey, 1, 3*time.Second).Result()
	if err != nil {
		fail(w, 500, "锁服务异常: "+err.Error())
		return
	}
	if !ok {
		fail(w, 429, "该货品正在被其他人锁定，请稍后重试")
		return
	}
	defer a.rdb.Del(r.Context(), lockKey)

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var (
		wholesalerID, batchID, avail int
		listPrice                    *float64
		listed                       bool
		invStatus                    string
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT wholesaler_id, batch_id, bundles_available, list_price, listed, status
		 FROM inventories WHERE id=$1 FOR UPDATE`, in.InventoryID).
		Scan(&wholesalerID, &batchID, &avail, &listPrice, &listed, &invStatus)
	if err == sql.ErrNoRows {
		fail(w, 404, "库存不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if !listed || invStatus != "in_stock" || listPrice == nil {
		fail(w, 409, "该货品未上架，暂不可采购")
		return
	}
	if avail < in.Bundles {
		fail(w, 409, fmt.Sprintf("库存不足：可售 %d 扎", avail))
		return
	}

	price := *listPrice // 下单时锁定价格，此后批发商改价不影响本单
	total := round2(price * float64(in.Bundles))
	deadline := time.Now().Add(time.Duration(a.cfg.PaymentTimeoutMin) * time.Minute)
	if in.IsPresale {
		deadline = time.Now().Add(time.Duration(a.cfg.PresalePaymentHours) * time.Hour)
	}

	var orderID int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO orders (shop_id, inventory_id, wholesaler_id, batch_id, bundles, price_per_bundle,
		  total_amount, purpose, payment_type, is_presale, festival, payment_deadline)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		u.ID, in.InventoryID, wholesalerID, batchID, in.Bundles, price, total,
		in.Purpose, in.PaymentType, in.IsPresale, in.Festival, deadline).Scan(&orderID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	orderNo := fmt.Sprintf("O%08d", orderID)
	if _, err := tx.ExecContext(r.Context(), `UPDATE orders SET order_no=$1 WHERE id=$2`, orderNo, orderID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE inventories SET bundles_available=bundles_available-$1, bundles_reserved=bundles_reserved+$1 WHERE id=$2`,
		in.Bundles, in.InventoryID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:market", "cache:overview")
	writeJSON(w, 201, map[string]any{
		"id": orderID, "order_no": orderNo, "status": "pending_payment",
		"total_amount": total, "payment_deadline": deadline,
	})
}

func orderRowMap() func(id, bundles int, orderNo, purpose, payType, status, festival, batchNo, cat, origin, shopName, wholeName string, price, total float64, presale bool, deadline, createdAt time.Time, paidAt, deliveredAt *time.Time, parentID *int, note string) map[string]any {
	return func(id, bundles int, orderNo, purpose, payType, status, festival, batchNo, cat, origin, shopName, wholeName string, price, total float64, presale bool, deadline, createdAt time.Time, paidAt, deliveredAt *time.Time, parentID *int, note string) map[string]any {
		return map[string]any{
			"id": id, "order_no": orderNo, "bundles": bundles, "purpose": purpose,
			"payment_type": payType, "status": status, "festival": festival, "is_presale": presale,
			"price_per_bundle": price, "total_amount": total, "payment_deadline": deadline,
			"paid_at": paidAt, "delivered_at": deliveredAt, "created_at": createdAt,
			"batch_no": batchNo, "category": cat, "origin": origin,
			"shop_name": shopName, "wholesaler_name": wholeName,
			"parent_order_id": parentID, "note": note,
		}
	}
}

const orderSelect = `
 SELECT o.id, o.bundles, COALESCE(o.order_no,''), o.purpose, o.payment_type, o.status, o.festival,
        b.batch_no, b.category, b.origin, s.name, w.name,
        o.price_per_bundle, o.total_amount, o.is_presale, o.payment_deadline, o.created_at,
        o.paid_at, o.delivered_at, o.parent_order_id, o.note
 FROM orders o
 JOIN flower_batches b ON b.id=o.batch_id
 JOIN users s ON s.id=o.shop_id
 JOIN users w ON w.id=o.wholesaler_id`

func scanOrder(rows *sql.Rows) (map[string]any, error) {
	var (
		id, bundles                                          int
		orderNo, purpose, payType, status, festival          string
		batchNo, cat, origin, shopName, wholeName, note      string
		price, total                                         float64
		presale                                              bool
		deadline, createdAt                                  time.Time
		paidAt, deliveredAt                                  *time.Time
		parentID                                             *int
	)
	err := rows.Scan(&id, &bundles, &orderNo, &purpose, &payType, &status, &festival,
		&batchNo, &cat, &origin, &shopName, &wholeName,
		&price, &total, &presale, &deadline, &createdAt, &paidAt, &deliveredAt, &parentID, &note)
	if err != nil {
		return nil, err
	}
	return orderRowMap()(id, bundles, orderNo, purpose, payType, status, festival, batchNo, cat, origin,
		shopName, wholeName, price, total, presale, deadline, createdAt, paidAt, deliveredAt, parentID, note), nil
}

// handleListOrders 花店看自己；批发商看自己售出的；财务/管理/配送看全部
func (a *app) handleListOrders(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := orderSelect
	args := []any{}
	switch u.Role {
	case "shop":
		q += " WHERE o.shop_id=$1"
		args = append(args, u.ID)
	case "wholesaler":
		q += " WHERE o.wholesaler_id=$1"
		args = append(args, u.ID)
	}
	if st := r.URL.Query().Get("status"); st != "" {
		if len(args) == 0 {
			q += " WHERE o.status=$1"
		} else {
			q += fmt.Sprintf(" AND o.status=$%d", len(args)+1)
		}
		args = append(args, st)
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY o.id DESC LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		if m, err := scanOrder(rows); err == nil {
			out = append(out, m)
		}
	}
	writeJSON(w, 200, out)
}

func (a *app) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), orderSelect+" WHERE o.id=$1", id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	if !rows.Next() {
		fail(w, 404, "订单不存在")
		return
	}
	m, err := scanOrder(rows)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 关联退货
	rets := []map[string]any{}
	r2, err := a.db.QueryContext(r.Context(),
		`SELECT id, bundles, reason, loss_ratio, claim_amount, status, created_at FROM returns WHERE order_id=$1 ORDER BY id DESC`, id)
	if err == nil {
		defer r2.Close()
		for r2.Next() {
			var rid, bundles int
			var reason, status string
			var ratio, claim float64
			var at time.Time
			if r2.Scan(&rid, &bundles, &reason, &ratio, &claim, &status, &at) == nil {
				rets = append(rets, map[string]any{"id": rid, "bundles": bundles, "reason": reason,
					"loss_ratio": ratio, "claim_amount": claim, "status": status, "created_at": at})
			}
		}
	}
	m["returns"] = rets
	// 拆分子单
	children := []map[string]any{}
	r3, err := a.db.QueryContext(r.Context(), orderSelect+" WHERE o.parent_order_id=$1 ORDER BY o.id", id)
	if err == nil {
		defer r3.Close()
		for r3.Next() {
			if c, err := scanOrder(r3); err == nil {
				children = append(children, c)
			}
		}
	}
	m["children"] = children
	writeJSON(w, 200, m)
}

// handlePayOrder 支付：立即支付或账期挂账（校验账期额度）
func (a *app) handlePayOrder(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var (
		shopID, invID, bundles int
		status, payType        string
		total                  float64
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT shop_id, inventory_id, bundles, status, payment_type, total_amount
		 FROM orders WHERE id=$1 FOR UPDATE`, id).
		Scan(&shopID, &invID, &bundles, &status, &payType, &total)
	if err == sql.ErrNoRows {
		fail(w, 404, "订单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if u.Role == "shop" && u.ID != shopID {
		fail(w, 403, "只能支付自己的订单")
		return
	}
	if status != "pending_payment" {
		fail(w, 409, "订单当前状态不可支付: "+status)
		return
	}

	method := "online"
	if payType == "credit" {
		// 账期客户：校验额度并挂应收
		var limit float64
		var days int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT credit_limit, credit_days FROM users WHERE id=$1`, shopID).Scan(&limit, &days); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if days == 0 {
			fail(w, 409, "该客户未开通账期")
			return
		}
		var used float64
		if err := tx.QueryRowContext(r.Context(),
			`SELECT COALESCE(SUM(amount),0) FROM credit_receivables WHERE customer_id=$1 AND status IN ('open','overdue')`,
			shopID).Scan(&used); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if used+total > limit {
			fail(w, 409, fmt.Sprintf("账期额度不足：已用 %.2f + 本单 %.2f 超过额度 %.2f", used, total, limit))
			return
		}
		due := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO credit_receivables (order_id, customer_id, amount, due_date) VALUES ($1,$2,$3,$4)`,
			id, shopID, total, due); err != nil {
			fail(w, 500, err.Error())
			return
		}
		method = "credit"
	}

	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO payments (order_id, amount, method, recorded_by) VALUES ($1,$2,$3,$4)`,
		id, total, method, u.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE orders SET status='paid', paid_at=now() WHERE id=$1`, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 锁定库存转为已售
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE inventories SET bundles_reserved=GREATEST(0, bundles_reserved-$1) WHERE id=$2`, bundles, invID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:market", "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": "paid", "method": method})
}

// handleCancelOrder 花店主动取消未付款订单，释放锁定
func (a *app) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var shopID, invID, bundles int
	var status string
	err = tx.QueryRowContext(r.Context(),
		`SELECT shop_id, inventory_id, bundles, status FROM orders WHERE id=$1 FOR UPDATE`, id).
		Scan(&shopID, &invID, &bundles, &status)
	if err == sql.ErrNoRows {
		fail(w, 404, "订单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if shopID != u.ID {
		fail(w, 403, "只能取消自己的订单")
		return
	}
	if status != "pending_payment" {
		fail(w, 409, "仅待付款订单可取消")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE orders SET status='cancelled' WHERE id=$1`, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE inventories SET bundles_reserved=GREATEST(0,bundles_reserved-$1), bundles_available=bundles_available+$1 WHERE id=$2`,
		bundles, invID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:market")
	writeJSON(w, 200, map[string]any{"id": id, "status": "cancelled"})
}

// handleSplitOrder 节日前订单拆分：拆出若干子单分别配送/结算
func (a *app) handleSplitOrder(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Children []struct {
			Bundles int    `json:"bundles"`
			Note    string `json:"note"`
		} `json:"children"`
	}
	if err := decodeBody(r, &in); err != nil || len(in.Children) == 0 {
		fail(w, 400, "请给出拆分方案 children:[{bundles,note}]")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var (
		shopID, bundles                                      int
		status, orderNo, purpose, payType, festival          string
		price                                                float64
		presale                                              bool
		deadline                                             time.Time
		paidAt                                               *time.Time
		invID, wholesalerID, batchID                         int
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT shop_id, bundles, status, COALESCE(order_no,''), purpose, payment_type, festival,
		        price_per_bundle, is_presale, payment_deadline, paid_at, inventory_id, wholesaler_id, batch_id
		 FROM orders WHERE id=$1 FOR UPDATE`, id).
		Scan(&shopID, &bundles, &status, &orderNo, &purpose, &payType, &festival,
			&price, &presale, &deadline, &paidAt, &invID, &wholesalerID, &batchID)
	if err == sql.ErrNoRows {
		fail(w, 404, "订单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if shopID != u.ID {
		fail(w, 403, "只能拆分自己的订单")
		return
	}
	if status != "pending_payment" && status != "paid" {
		fail(w, 409, "仅待付款或已付款（未配送）订单可拆分")
		return
	}
	sum := 0
	for _, c := range in.Children {
		if c.Bundles <= 0 {
			fail(w, 400, "子单扎数须大于 0")
			return
		}
		sum += c.Bundles
	}
	if sum >= bundles {
		fail(w, 409, fmt.Sprintf("拆出扎数须小于原单 %d 扎（剩余留在原单）", bundles))
		return
	}
	// 原单扣减
	remain := bundles - sum
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE orders SET bundles=$1, total_amount=$2 WHERE id=$3`,
		remain, round2(price*float64(remain)), id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	childIDs := []int{}
	for i, c := range in.Children {
		var cid int
		childStatus := status
		err := tx.QueryRowContext(r.Context(),
			`INSERT INTO orders (shop_id, inventory_id, wholesaler_id, batch_id, bundles, price_per_bundle,
			  total_amount, purpose, payment_type, status, is_presale, festival, parent_order_id, note,
			  payment_deadline, paid_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
			shopID, invID, wholesalerID, batchID, c.Bundles, price,
			round2(price*float64(c.Bundles)), purpose, payType, childStatus, presale, festival, id, c.Note,
			deadline, paidAt).Scan(&cid)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		childNo := fmt.Sprintf("%s-%d", orderNo, i+1)
		if _, err := tx.ExecContext(r.Context(), `UPDATE orders SET order_no=$1 WHERE id=$2`, childNo, cid); err != nil {
			fail(w, 500, err.Error())
			return
		}
		childIDs = append(childIDs, cid)
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"parent_id": id, "remaining_bundles": remain, "children": childIDs})
}
