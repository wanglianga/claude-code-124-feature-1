package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---------- 短枝损耗复核 ----------
//
// 业务闭环：
//  花店反馈短枝比例高于竞价说明 → 服务汇聚四组证据（分拣照片 / 质检等级 / 出库扎数 / 到店复拍）
//  → 质检确认并选择处理（差价补偿 / 退货 / 下一批折扣）→ 花农端确认账扣；
//  花农异议则两组证据保留、财务冻结差价，争议关闭前不得入账，关闭（维持/推翻）后继续流程。

const shortStemReviewSelect = `
 SELECT r.id, COALESCE(r.review_no,''), r.order_id, r.batch_id, r.shop_id, r.wholesaler_id, r.farmer_id,
        r.bundles, r.auction_declared_short_ratio, r.qc_grade_snapshot, r.outbound_bundles,
        r.sorting_photos, r.outbound_photo, r.actual_short_ratio, r.reshoot_photos, r.shop_remark,
        r.status, r.resolution, r.qc_note, r.confirmed_qc_at,
        r.price_diff_per_bundle, r.price_diff_amount, r.return_bundles, r.return_amount,
        r.next_discount_rate, r.commitment_id, r.frozen_amount,
        r.farmer_responsibility, r.farmer_charged, r.confirmed_at,
        r.dispute_raised_at, r.dispute_reason, r.closed_at, r.close_note, r.created_at,
        b.batch_no, b.category, b.origin, sh.name, w.name, f.name, COALESCE(o.order_no,'')
 FROM short_stem_reviews r
 JOIN flower_batches b ON b.id=r.batch_id
 JOIN users sh ON sh.id=r.shop_id
 JOIN users w ON w.id=r.wholesaler_id
 JOIN users f ON f.id=r.farmer_id
 JOIN orders o ON o.id=r.order_id`

func photosSplit(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func scanShortStemReview(rows *sql.Rows) (map[string]any, error) {
	var (
		id, orderID, batchID, shopID, wholesalerID, farmerID, bundles, outboundBundles int
		returnBundles                                                                  int
		commitmentID                                                                   *int
		reviewNo, grade, sortingPhotos, outboundPhoto, reshootPhotos, shopRemark       string
		declaredRatio, actualRatio                                                     float64
		status, resolution, qcNote                                                     string
		farmerConfirmedAt                                                              *time.Time
		createdAt                                                                      time.Time
		qcAt, disputeRaisedAt, closedAt                                                *time.Time
		pricePer, priceAmount, returnAmount, discountRate, frozen                      float64
		farmerResp, disputeReason, closeNote                                           string
		farmerCharged                                                                  bool
		batchNo, cat, origin, shopName, wholeName, farmerName, orderNo                 string
	)
	err := rows.Scan(&id, &reviewNo, &orderID, &batchID, &shopID, &wholesalerID, &farmerID,
		&bundles, &declaredRatio, &grade, &outboundBundles,
		&sortingPhotos, &outboundPhoto, &actualRatio, &reshootPhotos, &shopRemark,
		&status, &resolution, &qcNote, &qcAt,
		&pricePer, &priceAmount, &returnBundles, &returnAmount,
		&discountRate, &commitmentID, &frozen,
		&farmerResp, &farmerCharged, &farmerConfirmedAt,
		&disputeRaisedAt, &disputeReason, &closedAt, &closeNote, &createdAt,
		&batchNo, &cat, &origin, &shopName, &wholeName, &farmerName, &orderNo)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "review_no": reviewNo, "order_id": orderID, "batch_id": batchID,
		"order_no": orderNo, "batch_no": batchNo, "category": cat, "origin": origin,
		"shop_id": shopID, "wholesaler_id": wholesalerID, "farmer_id": farmerID,
		"shop_name": shopName, "wholesaler_name": wholeName, "farmer_name": farmerName,
		"bundles": bundles,
		// 第 1 组证据：到货质检/出库（分拣照片、质检等级、出库扎数）
		"evidence_arrival": map[string]any{
			"sorting_photos":       photosSplit(sortingPhotos),
			"outbound_photo":       outboundPhoto,
			"qc_grade":             grade,
			"declared_short_ratio": declaredRatio,
			"outbound_bundles":     outboundBundles,
		},
		// 第 2 组证据：到店复拍
		"evidence_store": map[string]any{
			"actual_short_ratio": actualRatio,
			"reshoot_photos":     photosSplit(reshootPhotos),
			"shop_remark":        shopRemark,
		},
		"status": status, "resolution": resolution, "qc_note": qcNote, "confirmed_qc_at": qcAt,
		"price_diff_per_bundle": pricePer, "price_diff_amount": priceAmount,
		"return_bundles": returnBundles, "return_amount": returnAmount,
		"next_discount_rate": discountRate, "commitment_id": commitmentID,
		"frozen_amount":         frozen,
		"farmer_responsibility": farmerResp, "farmer_charged": farmerCharged,
		"confirmed_at":      farmerConfirmedAt,
		"dispute_raised_at": disputeRaisedAt, "dispute_reason": disputeReason,
		"closed_at": closedAt, "close_note": closeNote, "created_at": createdAt,
	}, nil
}

// canViewReview 角色可见性：花农/花店/批发商仅本主体，质检/财务/管理可看全部
func canViewReview(u *User, m map[string]any) bool {
	switch u.Role {
	case "farmer":
		return m["farmer_id"] == u.ID
	case "shop":
		return m["shop_id"] == u.ID
	case "wholesaler":
		return m["wholesaler_id"] == u.ID
	case "qc", "finance", "manager":
		return true
	}
	return false
}

// handleCreateShortStemReview 花店就已配送/送达订单发起短枝损耗复核，服务自动汇聚四组证据
func (a *app) handleCreateShortStemReview(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	orderID, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Bundles          int      `json:"bundles"`
		ActualShortRatio float64  `json:"actual_short_ratio"` // 到店复拍实测短枝比例
		ReshootPhotos    []string `json:"reshoot_photos"`     // 到店复拍照片
		Remark           string   `json:"remark"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.Bundles <= 0 {
		fail(w, 400, "涉及扎数须大于 0")
		return
	}
	if in.ActualShortRatio <= 0 || in.ActualShortRatio > 1 {
		fail(w, 400, "实测短枝比例须在 (0,1]")
		return
	}
	reshoot := photosSplit(strings.Join(in.ReshootPhotos, ","))
	if len(reshoot) == 0 {
		fail(w, 400, "请上传至少 1 张到店复拍照片")
		return
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var (
		shopID, wholesalerID, batchID, orderBundles int
		status                                      string
		price                                       float64
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT shop_id, wholesaler_id, batch_id, bundles, status, price_per_bundle
		 FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&shopID, &wholesalerID, &batchID, &orderBundles, &status, &price)
	if err == sql.ErrNoRows {
		fail(w, 404, "订单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if shopID != u.ID {
		fail(w, 403, "只能对自己的订单发起短枝复核")
		return
	}
	if status != "dispatched" && status != "delivered" && status != "completed" {
		fail(w, 409, "仅配送中/已送达/已完成订单可发起短枝复核，当前: "+status)
		return
	}
	if in.Bundles > orderBundles {
		fail(w, 409, fmt.Sprintf("涉及扎数不能超过订单出库扎数 %d", orderBundles))
		return
	}
	var openBundles int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT COALESCE(SUM(bundles),0) FROM short_stem_reviews
		 WHERE order_id=$1 AND status IN ('pending_qc','confirmed','disputed','closed_upheld')`,
		orderID).Scan(&openBundles); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if openBundles+in.Bundles > orderBundles {
		fail(w, 409, fmt.Sprintf("该订单未结案复核已涉及 %d 扎，不能超过出库扎数 %d", openBundles, orderBundles))
		return
	}

	// 证据②质检等级与竞价说明短枝比例：取该批次最近一次分拣质检记录
	var grade string
	var declaredRatio float64
	err = tx.QueryRowContext(r.Context(),
		`SELECT grade, short_stem_ratio FROM qc_records WHERE batch_id=$1 ORDER BY id DESC LIMIT 1`,
		batchID).Scan(&grade, &declaredRatio)
	if err == sql.ErrNoRows {
		fail(w, 409, "该批次缺少分拣质检记录，无法核对竞价说明")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if in.ActualShortRatio <= declaredRatio {
		fail(w, 409, fmt.Sprintf("实测短枝比例 %.1f%% 未高于竞价说明 %.1f%%，无需复核",
			in.ActualShortRatio*100, declaredRatio*100))
		return
	}

	// 证据①分拣照片：取该订单最近一次配送（出库）记录；证据③出库扎数即订单出库扎数
	var sortingPhotos string
	err = tx.QueryRowContext(r.Context(),
		`SELECT COALESCE(sorting_photos,'') FROM deliveries
		 WHERE order_id=$1 ORDER BY id DESC LIMIT 1`, orderID).Scan(&sortingPhotos)
	if err == sql.ErrNoRows {
		fail(w, 409, "该订单尚无配送出库记录，缺少分拣照片证据")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if sortingPhotos == "" {
		fail(w, 409, "出库配送未拍摄分拣照片，证据不全，请联系配送补拍后再发起")
		return
	}
	outboundPhoto := ""
	if ps := photosSplit(sortingPhotos); len(ps) > 0 {
		outboundPhoto = ps[0] // 分拣首照兼作出库留影
	}
	outboundBundles := orderBundles

	var rid int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO short_stem_reviews
		   (order_id, batch_id, shop_id, wholesaler_id, farmer_id, bundles,
		    auction_declared_short_ratio, qc_grade_snapshot, outbound_bundles,
		    sorting_photos, outbound_photo, actual_short_ratio, reshoot_photos, shop_remark)
		 VALUES ($1,$2,$3,$4,(SELECT farmer_id FROM flower_batches WHERE id=$2),$5,$6,$7,$8,$9,$10,$11,$12,$13)
		 RETURNING id`,
		orderID, batchID, shopID, wholesalerID, in.Bundles,
		declaredRatio, grade, outboundBundles,
		sortingPhotos, outboundPhoto, in.ActualShortRatio, strings.Join(reshoot, ","), in.Remark).Scan(&rid)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	reviewNo := fmt.Sprintf("SSR%08d", rid)
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE short_stem_reviews SET review_no=$1 WHERE id=$2`, reviewNo, rid); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{
		"id": rid, "review_no": reviewNo, "status": "pending_qc",
		"declared_short_ratio": declaredRatio, "actual_short_ratio": in.ActualShortRatio,
	})
}

// handleListShortStemReviews 复核单列表（角色过滤；花农端据此同步账扣状态）
func (a *app) handleListShortStemReviews(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := shortStemReviewSelect
	args := []any{}
	where := ""
	switch u.Role {
	case "shop":
		where = " WHERE r.shop_id=$1"
		args = append(args, u.ID)
	case "farmer":
		where = " WHERE r.farmer_id=$1"
		args = append(args, u.ID)
	case "wholesaler":
		where = " WHERE r.wholesaler_id=$1"
		args = append(args, u.ID)
	}
	if st := r.URL.Query().Get("status"); st != "" {
		if where == "" {
			where = " WHERE r.status=$1"
		} else {
			where += fmt.Sprintf(" AND r.status=$%d", len(args)+1)
		}
		args = append(args, st)
	}
	rows, err := a.db.QueryContext(r.Context(), q+where+" ORDER BY r.id DESC LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	chargedTotal, frozenTotal := 0.0, 0.0
	for rows.Next() {
		m, err := scanShortStemReview(rows)
		if err != nil {
			continue
		}
		out = append(out, m)
		if m["farmer_charged"] == true {
			chargedTotal += m["price_diff_amount"].(float64) + m["return_amount"].(float64)
		}
		if m["status"] == "disputed" {
			frozenTotal += m["frozen_amount"].(float64)
		}
	}
	// 花农端账扣状态汇总（已确认账扣金额 / 争议冻结金额）
	sum := map[string]any{"charged_amount": round2(chargedTotal), "frozen_amount": round2(frozenTotal), "count": len(out)}
	writeJSON(w, 200, map[string]any{"items": out, "summary": sum})
}

// handleGetShortStemReview 复核单详情（含下一批折扣承诺核销情况）
func (a *app) handleGetShortStemReview(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), shortStemReviewSelect+" WHERE r.id=$1", id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	if !rows.Next() {
		fail(w, 404, "复核单不存在")
		return
	}
	m, err := scanShortStemReview(rows)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if !canViewReview(u, m) {
		fail(w, 403, "无权查看该复核单")
		return
	}
	if cid, ok := m["commitment_id"].(*int); ok && cid != nil {
		var rate, redeemed float64
		var cstatus string
		var auctionID *int
		var redeemedAt *time.Time
		if err := a.db.QueryRowContext(r.Context(),
			`SELECT rate, status, redeemed_auction_id, redeemed_amount, redeemed_at
			 FROM farmer_discount_commitments WHERE id=$1`, *cid).
			Scan(&rate, &cstatus, &auctionID, &redeemed, &redeemedAt); err == nil {
			m["commitment"] = map[string]any{
				"id": *cid, "rate": rate, "status": cstatus,
				"redeemed_auction_id": auctionID, "redeemed_amount": redeemed, "redeemed_at": redeemedAt,
			}
		}
	}
	writeJSON(w, 200, m)
}

// handleConfirmShortStemReview 质检复核确认：差价补偿 / 退货 / 下一批折扣
func (a *app) handleConfirmShortStemReview(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Resolution         string   `json:"resolution"` // price_diff/return/next_discount
		PriceDiffPerBundle *float64 `json:"price_diff_per_bundle"`
		ReturnBundles      int      `json:"return_bundles"`
		NextDiscountRate   float64  `json:"next_discount_rate"`
		Note               string   `json:"note"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.Resolution != "price_diff" && in.Resolution != "return" && in.Resolution != "next_discount" {
		fail(w, 400, "处理方式 resolution 须为 price_diff(差价补偿)/return(退货)/next_discount(下一批折扣)")
		return
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var (
		status                  string
		bundles                 int
		orderPrice, actual, dec float64
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT r.status, r.bundles, r.actual_short_ratio, r.auction_declared_short_ratio, o.price_per_bundle
		 FROM short_stem_reviews r JOIN orders o ON o.id=r.order_id
		 WHERE r.id=$1 FOR UPDATE OF r`, id).
		Scan(&status, &bundles, &actual, &dec, &orderPrice)
	if err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "pending_qc" {
		fail(w, 409, "该复核单已确认，当前状态: "+status)
		return
	}

	perBundle, priceAmount := 0.0, 0.0
	retBundles, retAmount := 0, 0.0
	discountRate := 0.0
	var commitmentID *int

	switch in.Resolution {
	case "price_diff":
		perBundle = orderPrice * (actual - dec) // 默认差价口径：单价 × 短枝比例差
		if in.PriceDiffPerBundle != nil && *in.PriceDiffPerBundle > 0 {
			perBundle = *in.PriceDiffPerBundle
		}
		priceAmount = round2(perBundle * float64(bundles))
		if priceAmount <= 0 {
			fail(w, 400, "差价补偿金额须大于 0")
			return
		}
	case "return":
		retBundles = in.ReturnBundles
		if retBundles <= 0 {
			retBundles = bundles
		}
		if retBundles > bundles {
			fail(w, 409, fmt.Sprintf("退货扎数不能超过复核扎数 %d", bundles))
			return
		}
		retAmount = round2(orderPrice * float64(retBundles))
	case "next_discount":
		discountRate = in.NextDiscountRate
		if discountRate <= 0 || discountRate > 0.5 {
			fail(w, 400, "下一批折扣率须在 (0,0.5]，如 0.10 表示让利 10%")
			return
		}
		var farmerID int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT farmer_id FROM short_stem_reviews WHERE id=$1`, id).Scan(&farmerID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		var cid int
		if err := tx.QueryRowContext(r.Context(),
			`INSERT INTO farmer_discount_commitments (farmer_id, review_id, rate)
			 VALUES ($1,$2,$3) RETURNING id`, farmerID, id, discountRate).Scan(&cid); err != nil {
			fail(w, 500, err.Error())
			return
		}
		commitmentID = &cid
	}

	if _, err := tx.ExecContext(r.Context(),
		`UPDATE short_stem_reviews
		 SET status='confirmed', resolution=$1, qc_note=$2, confirmed_qc_by=$3, confirmed_qc_at=now(),
		     price_diff_per_bundle=$4, price_diff_amount=$5,
		     return_bundles=$6, return_amount=$7, next_discount_rate=$8, commitment_id=COALESCE($9, commitment_id)
		 WHERE id=$10`,
		in.Resolution, in.Note, u.ID, round2(perBundle), priceAmount,
		retBundles, retAmount, discountRate, commitmentID, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{
		"id": id, "status": "confirmed", "resolution": in.Resolution,
		"price_diff_amount": priceAmount, "return_amount": retAmount,
		"return_bundles": retBundles, "next_discount_rate": discountRate,
		"commitment_id": commitmentID,
	})
}

// handleAckShortStemCharge 花农端确认账扣（账扣状态同步花农端）
func (a *app) handleAckShortStemCharge(w http.ResponseWriter, r *http.Request) {
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
	var farmerID int
	var status string
	err = tx.QueryRowContext(r.Context(),
		`SELECT farmer_id, status FROM short_stem_reviews WHERE id=$1 FOR UPDATE`, id).
		Scan(&farmerID, &status)
	if err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if farmerID != u.ID {
		fail(w, 403, "只能确认自己名下批次的账扣")
		return
	}
	if status != "confirmed" && status != "closed_upheld" {
		fail(w, 409, "当前状态不可确认账扣: "+status)
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE short_stem_reviews
		 SET status='charged', farmer_responsibility='yes', farmer_charged=true,
		     confirmed_by=$1, confirmed_at=now(), farmer_seen_at=COALESCE(farmer_seen_at,now())
		 WHERE id=$2`, u.ID, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": "charged", "farmer_charged": true})
}

// handleDisputeShortStem 花农对损耗判定提出异议：两组证据保留，财务冻结相关差价
func (a *app) handleDisputeShortStem(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if strings.TrimSpace(in.Reason) == "" {
		fail(w, 400, "请填写异议理由")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var farmerID int
	var status string
	var priceAmount, returnAmount float64
	err = tx.QueryRowContext(r.Context(),
		`SELECT farmer_id, status, price_diff_amount, return_amount
		 FROM short_stem_reviews WHERE id=$1 FOR UPDATE`, id).
		Scan(&farmerID, &status, &priceAmount, &returnAmount)
	if err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if farmerID != u.ID {
		fail(w, 403, "只能对自己名下批次的判定提出异议")
		return
	}
	if status != "confirmed" {
		fail(w, 409, "仅已质检确认、待账扣的复核单可提出异议，当前: "+status)
		return
	}
	frozen := round2(priceAmount + returnAmount) // 冻结差价；下一批折扣承诺保持待核销不生效
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE short_stem_reviews
		 SET status='disputed', farmer_responsibility='no', dispute_raised_at=now(),
		     dispute_reason=$1, frozen_amount=$2, farmer_seen_at=COALESCE(farmer_seen_at,now())
		 WHERE id=$3`, in.Reason, frozen, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": "disputed", "frozen_amount": frozen})
}

// handleCloseShortStemDispute 财务/质检/管理关闭争议：维持原判（继续账扣）或推翻（解冻、承诺作废）
func (a *app) handleCloseShortStemDispute(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Uphold bool   `json:"uphold"`
		Note   string `json:"note"`
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
	var status string
	var commitmentID *int
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, commitment_id FROM short_stem_reviews WHERE id=$1 FOR UPDATE`, id).
		Scan(&status, &commitmentID)
	if err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "disputed" {
		fail(w, 409, "该复核单不在争议中，当前: "+status)
		return
	}
	if in.Uphold {
		// 维持原判：解冻标记保留可追溯，回到待花农账扣确认
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE short_stem_reviews
			 SET status='closed_upheld', closed_by=$1, closed_at=now(), close_note=$2
			 WHERE id=$3`, u.ID, in.Note, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
	} else {
		// 推翻：差价解冻不账扣；若已生成下一批折扣承诺则作废
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE short_stem_reviews
			 SET status='closed_overturned', closed_by=$1, closed_at=now(), close_note=$2,
			     frozen_amount=0, farmer_responsibility='no'
			 WHERE id=$3`, u.ID, in.Note, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if commitmentID != nil {
			if _, err := tx.ExecContext(r.Context(),
				`UPDATE farmer_discount_commitments SET status='voided' WHERE id=$1 AND status='pending'`,
				*commitmentID); err != nil {
				fail(w, 500, err.Error())
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	newStatus := "closed_upheld"
	if !in.Uphold {
		newStatus = "closed_overturned"
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": newStatus, "uphold": in.Uphold})
}
