package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------- 配送调度 ----------

// handleDispatch 配送发货：订单出库，扣减冷库占用
func (a *app) handleDispatch(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		OrderID       int    `json:"order_id"`
		ToCity        string `json:"to_city"`
		SortingPhotos string `json:"sorting_photos"` // 出库分拣照片（URL，多张逗号分隔）——短枝复核第 1 组证据
	}
	if err := decodeBody(r, &in); err != nil || in.OrderID == 0 || in.ToCity == "" {
		fail(w, 400, "请给出 order_id 与目的城市 to_city")
		return
	}
	sortingPhotos := strings.TrimSpace(in.SortingPhotos)
	if sortingPhotos == "" {
		fail(w, 400, "出库必须拍摄并上传分拣照片 sorting_photos（短枝复核证据）")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var (
		status         string
		bundles, invID int
		batchID        int
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, bundles, inventory_id, batch_id FROM orders WHERE id=$1 FOR UPDATE`, in.OrderID).
		Scan(&status, &bundles, &invID, &batchID)
	if err == sql.ErrNoRows {
		fail(w, 404, "订单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "paid" {
		fail(w, 409, "仅已付款订单可发货，当前: "+status)
		return
	}
	var deliveryID int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO deliveries (order_id, dispatcher_id, to_city, status, departed_at, sorting_photos)
		 VALUES ($1,$2,$3,'in_transit',now(),$4) RETURNING id`,
		in.OrderID, u.ID, in.ToCity, sortingPhotos).Scan(&deliveryID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE orders SET status='dispatched' WHERE id=$1`, in.OrderID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 货物离库：扣减冷库占用
	var slotID *int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT storage_slot_id FROM flower_batches WHERE id=$1`, batchID).Scan(&slotID); err == nil && slotID != nil {
		tx.ExecContext(r.Context(),
			`UPDATE storage_slots SET occupied_bundles=GREATEST(0, occupied_bundles-$1) WHERE id=$2`,
			bundles, *slotID)
	}
	// 库存售罄则批次出清，记录出库时间（冷库时长计费终点）
	var remain int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT bundles_available+bundles_reserved FROM inventories WHERE id=$1`, invID).Scan(&remain); err == nil && remain == 0 {
		tx.ExecContext(r.Context(), `UPDATE inventories SET status='sold_out' WHERE id=$1`, invID)
		tx.ExecContext(r.Context(),
			`UPDATE flower_batches SET out_at=now(), status='closed' WHERE id=$1 AND out_at IS NULL`, batchID)
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{"id": deliveryID, "order_id": in.OrderID, "status": "in_transit"})
}

func (a *app) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT d.id, d.order_id, d.transfer_id, d.to_city, d.status, d.departed_at, d.delivered_at, d.created_at,
		        COALESCE(d.sorting_photos,''), COALESCE(o.order_no,''), COALESCE(u.name,'')
		 FROM deliveries d
		 LEFT JOIN orders o ON o.id=d.order_id
		 LEFT JOIN users u ON u.id=d.dispatcher_id
		 ORDER BY d.id DESC LIMIT 200`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id                                 int
			orderID, transferID                *int
			city, status                       string
			departed, delivered                *time.Time
			createdAt                          time.Time
			orderNo, dispatcher, sortingPhotos string
		)
		if rows.Scan(&id, &orderID, &transferID, &city, &status, &departed, &delivered, &createdAt,
			&sortingPhotos, &orderNo, &dispatcher) == nil {
			out = append(out, map[string]any{
				"id": id, "order_id": orderID, "transfer_id": transferID, "to_city": city,
				"status": status, "departed_at": departed, "delivered_at": delivered,
				"created_at": createdAt, "sorting_photos": photosSplit(sortingPhotos),
				"order_no": orderNo, "dispatcher": dispatcher,
			})
		}
	}
	writeJSON(w, 200, out)
}

// handleDeliveryStatus 配送状态推进：in_transit / delivered（到店）
func (a *app) handleDeliveryStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := decodeBody(r, &in); err != nil || (in.Status != "in_transit" && in.Status != "delivered") {
		fail(w, 400, "status 须为 in_transit 或 delivered")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var orderID, transferID *int
	var cur string
	err = tx.QueryRowContext(r.Context(),
		`SELECT order_id, transfer_id, status FROM deliveries WHERE id=$1 FOR UPDATE`, id).
		Scan(&orderID, &transferID, &cur)
	if err == sql.ErrNoRows {
		fail(w, 404, "配送单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if cur == "delivered" {
		fail(w, 409, "配送单已送达")
		return
	}
	if in.Status == "delivered" {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE deliveries SET status='delivered', delivered_at=now() WHERE id=$1`, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if orderID != nil {
			if _, err := tx.ExecContext(r.Context(),
				`UPDATE orders SET status='delivered', delivered_at=now() WHERE id=$1`, *orderID); err != nil {
				fail(w, 500, err.Error())
				return
			}
		}
		if transferID != nil {
			// 跨城调货到达：核减本市场库存总量
			var invID, bundles int
			if err := tx.QueryRowContext(r.Context(),
				`SELECT inventory_id, bundles FROM transfers WHERE id=$1`, *transferID).Scan(&invID, &bundles); err == nil {
				tx.ExecContext(r.Context(), `UPDATE transfers SET status='arrived' WHERE id=$1`, *transferID)
				tx.ExecContext(r.Context(),
					`UPDATE inventories SET bundles_total=GREATEST(0,bundles_total-$1),
					   status=CASE WHEN bundles_total-$1<=0 AND bundles_available=0 THEN 'transferred_out' ELSE status END
					 WHERE id=$2`, bundles, invID)
			}
		}
	} else {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE deliveries SET status='in_transit', departed_at=COALESCE(departed_at,now()) WHERE id=$1`, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if transferID != nil {
			tx.ExecContext(r.Context(), `UPDATE transfers SET status='in_transit' WHERE id=$1`, *transferID)
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": in.Status})
}

// ---------- 退货（到店掉瓣等）与花农信用 ----------

// handleCreateReturn 花店发起退货（到店掉瓣/品质/失温/短枝）
func (a *app) handleCreateReturn(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	orderID, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Bundles   int     `json:"bundles"`
		Reason    string  `json:"reason"`
		LossRatio float64 `json:"loss_ratio"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.Bundles <= 0 || in.LossRatio <= 0 || in.LossRatio > 1 {
		fail(w, 400, "扎数须>0，损耗比例须在 (0,1]")
		return
	}
	if !contains(returnReasons, in.Reason) {
		fail(w, 400, "退货原因须为 petal_drop/quality/temp_damage/short_stem/other")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var (
		shopID, batchID, bundles int
		status                   string
		price                    float64
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT shop_id, batch_id, bundles, status, price_per_bundle FROM orders WHERE id=$1 FOR UPDATE`,
		orderID).Scan(&shopID, &batchID, &bundles, &status, &price)
	if err == sql.ErrNoRows {
		fail(w, 404, "订单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if shopID != u.ID {
		fail(w, 403, "只能对自己的订单发起退货")
		return
	}
	if status != "delivered" && status != "dispatched" {
		fail(w, 409, "仅配送中/已送达订单可退货，当前: "+status)
		return
	}
	var already int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT COALESCE(SUM(bundles),0) FROM returns WHERE order_id=$1 AND status<>'rejected'`,
		orderID).Scan(&already); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if already+in.Bundles > bundles {
		fail(w, 409, fmt.Sprintf("退货扎数超出订单量：已申请 %d 扎，订单共 %d 扎", already, bundles))
		return
	}
	claim := round2(float64(in.Bundles) * price * in.LossRatio)
	var rid int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO returns (order_id, batch_id, shop_id, bundles, reason, loss_ratio, claim_amount)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		orderID, batchID, u.ID, in.Bundles, in.Reason, in.LossRatio, claim).Scan(&rid)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{"id": rid, "order_id": orderID, "claim_amount": claim, "status": "requested"})
}

func (a *app) handleListReturns(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT r.id, r.order_id, r.bundles, r.reason, r.loss_ratio, r.claim_amount, r.status,
	             r.farmer_responsible, r.review_note, r.created_at, r.reviewed_at,
	             b.batch_no, b.category, s.name, COALESCE(o.order_no,'')
	      FROM returns r
	      JOIN flower_batches b ON b.id=r.batch_id
	      JOIN users s ON s.id=r.shop_id
	      LEFT JOIN orders o ON o.id=r.order_id`
	args := []any{}
	switch u.Role {
	case "shop":
		q += " WHERE r.shop_id=$1"
		args = append(args, u.ID)
	case "wholesaler":
		q += " WHERE o.wholesaler_id=$1"
		args = append(args, u.ID)
	case "farmer":
		q += " WHERE b.farmer_id=$1"
		args = append(args, u.ID)
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY r.id DESC LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			rid, orderID, bundles  int
			reason, status, note   string
			ratio, claim           float64
			responsible            bool
			createdAt              time.Time
			reviewedAt             *time.Time
			batchNo, cat, shop, no string
		)
		if rows.Scan(&rid, &orderID, &bundles, &reason, &ratio, &claim, &status, &responsible,
			&note, &createdAt, &reviewedAt, &batchNo, &cat, &shop, &no) == nil {
			out = append(out, map[string]any{
				"id": rid, "order_id": orderID, "order_no": no, "bundles": bundles, "reason": reason,
				"loss_ratio": ratio, "claim_amount": claim, "status": status,
				"farmer_responsible": responsible, "review_note": note,
				"created_at": createdAt, "reviewed_at": reviewedAt,
				"batch_no": batchNo, "category": cat, "shop_name": shop,
			})
		}
	}
	writeJSON(w, 200, out)
}

// handleReviewReturn 质检裁定退货：成立则赔付；判定花农责任则扣信用、影响下次竞价排序
func (a *app) handleReviewReturn(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Approve           bool   `json:"approve"`
		FarmerResponsible bool   `json:"farmer_responsible"`
		Note              string `json:"note"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var (
		status  string
		batchID int
		ratio   float64
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, batch_id, loss_ratio FROM returns WHERE id=$1 FOR UPDATE`, id).
		Scan(&status, &batchID, &ratio)
	if err == sql.ErrNoRows {
		fail(w, 404, "退货单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "requested" {
		fail(w, 409, "该退货单已裁定")
		return
	}
	newStatus := "rejected"
	if in.Approve {
		newStatus = "approved"
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE returns SET status=$1, reviewed_by=$2, reviewed_at=now(), farmer_responsible=$3, review_note=$4
		 WHERE id=$5`, newStatus, u.ID, in.Approve && in.FarmerResponsible, in.Note, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	creditDelta := 0
	var farmerID int
	if in.Approve && in.FarmerResponsible {
		// 按损耗比例扣花农信用：每 10% 损耗扣 1 分，至少 1 分
		creditDelta = -max(1, int(ratio*10+0.5))
		if err := tx.QueryRowContext(r.Context(),
			`SELECT farmer_id FROM flower_batches WHERE id=$1`, batchID).Scan(&farmerID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO farmer_credit_events (farmer_id, delta, reason, return_id) VALUES ($1,$2,$3,$4)`,
			farmerID, creditDelta, fmt.Sprintf("退货判定花农责任（退货单 #%d）", id), id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE users SET credit_score=GREATEST(0, credit_score+$1) WHERE id=$2`, creditDelta, farmerID); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if creditDelta != 0 {
		var score int
		if err := a.db.QueryRowContext(r.Context(),
			`SELECT credit_score FROM users WHERE id=$1`, farmerID).Scan(&score); err == nil {
			a.rdb.ZAdd(r.Context(), "farmer:credit", redis.Z{Score: float64(score), Member: farmerID})
		}
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": newStatus, "credit_delta": creditDelta})
}

// handleReturnReconciliation 退货对账（批发商/财务月底结算依据）
func (a *app) handleReturnReconciliation(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	month := r.URL.Query().Get("month") // YYYY-MM，缺省全部
	q := `SELECT r.id, r.bundles, r.reason, r.loss_ratio, r.claim_amount, r.status, r.farmer_responsible,
	             r.created_at, r.reviewed_at, b.batch_no, b.category, b.origin,
	             s.name, w.name, COALESCE(o.order_no,'')
	      FROM returns r
	      JOIN flower_batches b ON b.id=r.batch_id
	      JOIN users s ON s.id=r.shop_id
	      JOIN orders o ON o.id=r.order_id
	      JOIN users w ON w.id=o.wholesaler_id`
	args := []any{}
	where := ""
	if u.Role == "wholesaler" {
		where = " WHERE o.wholesaler_id=$1"
		args = append(args, u.ID)
	}
	if month != "" {
		if t, err := time.Parse("2006-01", month); err == nil {
			end := t.AddDate(0, 1, 0)
			if where == "" {
				where = " WHERE"
			} else {
				where += " AND"
			}
			where += fmt.Sprintf(" r.created_at >= $%d AND r.created_at < $%d", len(args)+1, len(args)+2)
			args = append(args, t, end)
		}
	}
	rows, err := a.db.QueryContext(r.Context(), q+where+" ORDER BY r.id DESC LIMIT 500", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	total := 0.0
	for rows.Next() {
		var (
			id, bundles          int
			reason, status       string
			ratio, claim         float64
			responsible          bool
			createdAt            time.Time
			reviewedAt           *time.Time
			batchNo, cat, origin string
			shop, whole, orderNo string
		)
		if rows.Scan(&id, &bundles, &reason, &ratio, &claim, &status, &responsible,
			&createdAt, &reviewedAt, &batchNo, &cat, &origin, &shop, &whole, &orderNo) == nil {
			if status == "approved" {
				total += claim
			}
			out = append(out, map[string]any{
				"id": id, "bundles": bundles, "reason": reason, "loss_ratio": ratio,
				"claim_amount": claim, "status": status, "farmer_responsible": responsible,
				"created_at": createdAt, "reviewed_at": reviewedAt,
				"batch_no": batchNo, "category": cat, "origin": origin,
				"shop_name": shop, "wholesaler_name": whole, "order_no": orderNo,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"month": month, "approved_claim_total": round2(total), "items": out})
}
