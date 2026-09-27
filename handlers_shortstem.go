package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// 短枝损耗复核处置方式
var reviewDispositions = []string{"price_compensation", "return_goods", "next_batch_discount"}

// 到货质检组证据（服务保留）：分拣照片 / 质检等级 / 出库扎数
var arrivalEvidenceTypes = map[string]bool{
	"sorting_photo":    true,
	"qc_grade":         true,
	"outbound_bundles": true,
}

// evidenceGroup 证据归属组：到货质检组 / 到店复拍组（两组均长期保留）
func evidenceGroup(t string) string {
	if arrivalEvidenceTypes[t] {
		return "arrival_qc"
	}
	return "shop_recheck"
}

// handleCreateShortStemReview 花店收到玫瑰后反馈短枝比例高于竞价说明，发起复核。
// 服务自动调取到货质检组证据（分拣照片/质检等级/出库扎数），花店提交到店复拍组证据。
func (a *app) handleCreateShortStemReview(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	orderID, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Bundles         int     `json:"bundles"`           // 反馈短枝扎数（缺省=整单）
		ObservedRatio   float64 `json:"observed_ratio"`    // 到店复拍短枝比例
		ShopPhotoURL    string  `json:"shop_photo_url"`    // 到店复拍照片
		SortingPhotoURL string  `json:"sorting_photo_url"` // 分拣照片（服务调取，可由花店/管理方补录）
		Note            string  `json:"note"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.ObservedRatio <= 0 || in.ObservedRatio > 1 {
		fail(w, 400, "到店复拍短枝比例须在 (0,1]")
		return
	}
	if in.ShopPhotoURL == "" {
		fail(w, 400, "请上传到店复拍照片（shop_photo_url）")
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
		status, orderNo                             string
		price                                       float64
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT shop_id, wholesaler_id, batch_id, bundles, status, price_per_bundle, COALESCE(order_no,'')
		 FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&shopID, &wholesalerID, &batchID, &orderBundles, &status, &price, &orderNo)
	if err == sql.ErrNoRows {
		fail(w, 404, "订单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if shopID != u.ID {
		fail(w, 403, "只能对自己收到的订单发起短枝复核")
		return
	}
	if status != "delivered" && status != "completed" {
		fail(w, 409, "仅已送达订单可发起短枝复核，当前: "+status)
		return
	}
	bundles := in.Bundles
	if bundles <= 0 {
		bundles = orderBundles
	}
	if bundles > orderBundles {
		fail(w, 409, fmt.Sprintf("复核扎数不能超过订单量 %d 扎", orderBundles))
		return
	}

	// 调取批次归属与竞价说明（到货质检记录的短枝比例/等级）
	var farmerID int
	var category string
	if err := tx.QueryRowContext(r.Context(),
		`SELECT farmer_id, category FROM flower_batches WHERE id=$1`, batchID).Scan(&farmerID, &category); err != nil {
		fail(w, 500, err.Error())
		return
	}
	var grade string
	var declared sql.NullFloat64
	if err := tx.QueryRowContext(r.Context(),
		`SELECT grade, short_stem_ratio FROM qc_records WHERE batch_id=$1 ORDER BY id DESC LIMIT 1`,
		batchID).Scan(&grade, &declared); err != nil {
		fail(w, 400, "该批次缺少到货质检记录，无法调取竞价说明短枝比例")
		return
	}
	declaredRatio := 0.0
	if declared.Valid {
		declaredRatio = declared.Float64
	}
	if in.ObservedRatio <= declaredRatio {
		fail(w, 409, fmt.Sprintf("到店复拍短枝比例 %.1f%% 未高于竞价说明 %.1f%%，无需复核",
			in.ObservedRatio*100, declaredRatio*100))
		return
	}

	// 同一订单只允许一笔进行中的复核
	var active int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT count(*) FROM short_stem_reviews WHERE order_id=$1
		 AND status IN ('requested','confirmed','disputed','acknowledged')`, orderID).Scan(&active); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if active > 0 {
		fail(w, 409, "该订单已有进行中的短枝复核单")
		return
	}

	var rid int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO short_stem_reviews
		 (order_id, batch_id, shop_id, wholesaler_id, farmer_id, bundles,
		  declared_short_ratio, observed_short_ratio, price_per_bundle, shop_note)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		orderID, batchID, u.ID, wholesalerID, farmerID, bundles,
		declaredRatio, in.ObservedRatio, price, in.Note).Scan(&rid)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	reviewNo := fmt.Sprintf("SR%08d", rid)
	tx.ExecContext(r.Context(), `UPDATE short_stem_reviews SET review_no=$1 WHERE id=$2`, reviewNo, rid)

	// —— 到货质检组证据（服务调取，长期保留，争议期间不可删改） ——
	addEvidenceTx(tx, rid, "arrival_qc", "qc_grade", "",
		fmt.Sprintf("到货质检等级 %s，登记短枝比例 %.1f%%", grade, declaredRatio*100), u.ID)
	addEvidenceTx(tx, rid, "arrival_qc", "outbound_bundles", "",
		fmt.Sprintf("订单 %s 出库扎数 %d 扎，本次反馈涉及 %d 扎", orderNo, orderBundles, bundles), u.ID)
	if in.SortingPhotoURL != "" {
		addEvidenceTx(tx, rid, "arrival_qc", "sorting_photo", in.SortingPhotoURL,
			"分拣线照片（发起时调取）", u.ID)
	}
	// —— 到店复拍组证据 ——
	addEvidenceTx(tx, rid, "shop_recheck", "shop_photo", in.ShopPhotoURL,
		fmt.Sprintf("到店复拍短枝比例 %.1f%%", in.ObservedRatio*100), u.ID)

	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{
		"id": rid, "review_no": reviewNo, "status": "requested",
		"declared_short_ratio": declaredRatio, "observed_short_ratio": in.ObservedRatio,
	})
}

func addEvidenceTx(tx *sql.Tx, reviewID int, group, typ, photoURL, detail string, uploaderID int) {
	tx.Exec(
		`INSERT INTO review_evidences (review_id, group_name, evidence_type, photo_url, detail, uploaded_by)
		 VALUES ($1,$2,$3,$4,$5,$6)`, reviewID, group, typ, photoURL, detail, uploaderID)
}

// handleAddReviewEvidence 补传证据：花店仅可补到店复拍（待确认阶段）；质检/管理方可补任意组
func (a *app) handleAddReviewEvidence(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		EvidenceType string `json:"evidence_type"`
		PhotoURL     string `json:"photo_url"`
		Detail       string `json:"detail"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	valid := arrivalEvidenceTypes[in.EvidenceType] || in.EvidenceType == "shop_photo"
	if !valid {
		fail(w, 400, "证据类型须为 sorting_photo/qc_grade/outbound_bundles/shop_photo")
		return
	}
	if in.PhotoURL == "" && in.Detail == "" {
		fail(w, 400, "照片地址与说明至少填一项")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var status string
	var shopID int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT status, shop_id FROM short_stem_reviews WHERE id=$1 FOR UPDATE`, id).
		Scan(&status, &shopID); err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "requested" {
		fail(w, 409, "质检已确认，两组证据已封存，不可再补传")
		return
	}
	if u.Role == "shop" {
		if shopID != u.ID {
			fail(w, 403, "只能给自己的复核单补传证据")
			return
		}
		if in.EvidenceType != "shop_photo" {
			fail(w, 403, "花店仅可补传到店复拍照片")
			return
		}
	} else if u.Role != "qc" && u.Role != "manager" {
		fail(w, 403, "当前角色无权补传证据")
		return
	}
	addEvidenceTx(tx, id, evidenceGroup(in.EvidenceType), in.EvidenceType, in.PhotoURL, in.Detail, u.ID)
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"review_id": id, "evidence_type": in.EvidenceType, "group": evidenceGroup(in.EvidenceType)})
}

// handleConfirmShortStemReview 质检复核确认：调取两组证据比对后给出实测短枝比例与处置方式。
// 处置三选一：差价补偿 / 退货 / 下一批折扣；花农结算同步受影响。
func (a *app) handleConfirmShortStemReview(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Confirm             bool    `json:"confirm"`
		ObservedRatio       float64 `json:"observed_ratio"`
		Disposition         string  `json:"disposition"`
		CompensationAmount  float64 `json:"compensation_amount"`
		NextDiscountPercent float64 `json:"next_discount_percent"`
		Note                string  `json:"note"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.Confirm {
		if in.ObservedRatio <= 0 || in.ObservedRatio > 1 {
			fail(w, 400, "复核实测短枝比例须在 (0,1]")
			return
		}
		if !contains(reviewDispositions, in.Disposition) {
			fail(w, 400, "处置方式须为 price_compensation/return_goods/next_batch_discount")
			return
		}
		if in.Disposition == "next_batch_discount" && (in.NextDiscountPercent <= 0 || in.NextDiscountPercent >= 100) {
			fail(w, 400, "下一批折扣比例须在 (0,100)")
			return
		}
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var (
		status                                      string
		batchID, farmerID, shopID, bundles, orderID int
		declared, observed, price                   float64
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, batch_id, farmer_id, shop_id, bundles, declared_short_ratio, observed_short_ratio,
		        price_per_bundle, order_id
		 FROM short_stem_reviews WHERE id=$1 FOR UPDATE`, id).
		Scan(&status, &batchID, &farmerID, &shopID, &bundles, &declared, &observed, &price, &orderID)
	if err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "requested" {
		fail(w, 409, "该复核单已处理，当前: "+status)
		return
	}
	if !in.Confirm {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE short_stem_reviews SET status='rejected', confirmed_by=$1, confirmed_at=now(), close_note=$2
			 WHERE id=$3`, u.ID, in.Note, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			fail(w, 500, err.Error())
			return
		}
		a.rdb.Del(r.Context(), "cache:overview")
		writeJSON(w, 200, map[string]any{"id": id, "status": "rejected"})
		return
	}
	if in.ObservedRatio <= declared {
		fail(w, 409, fmt.Sprintf("实测短枝比例 %.1f%% 未高于竞价说明 %.1f%%，复核不成立",
			in.ObservedRatio*100, declared*100))
		return
	}

	compensation := 0.0
	var linkedReturn *int
	// 短枝高于竞价说明即花农端品质责任：三种处置均同步扣减花农信用（影响下次竞价排序）
	creditDelta := -max(1, int(in.ObservedRatio*10+0.5))
	switch in.Disposition {
	case "price_compensation":
		// 差价补偿口径：扎数 × 每扎单价 × 高出的短枝比例
		compensation = round2(float64(bundles) * price * (in.ObservedRatio - declared))
		if in.CompensationAmount > 0 {
			compensation = round2(in.CompensationAmount)
		}
		if compensation <= 0 {
			fail(w, 409, "差价补偿金额为 0，请选择其他处置方式")
			return
		}
	case "return_goods":
		// 退货处置：生成短枝退货单，质检直接裁定成立并判花农责任（沿用退货扣款与信用扣分）
		claim := round2(float64(bundles) * price * in.ObservedRatio)
		var retID int
		if err := tx.QueryRowContext(r.Context(),
			`INSERT INTO returns (order_id, batch_id, shop_id, bundles, reason, loss_ratio, claim_amount,
			                      status, reviewed_by, reviewed_at, farmer_responsible, review_note)
			 VALUES ($1,$2,$3,$4,'short_stem',$5,$6,'approved',$7,now(),true,$8) RETURNING id`,
			orderID, batchID, shopID, bundles, in.ObservedRatio, claim, u.ID,
			fmt.Sprintf("短枝损耗复核单 #%d 确认退货", id)).Scan(&retID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		linkedReturn = &retID
	}

	if err := deductFarmerCreditTx(tx, farmerID, creditDelta,
		fmt.Sprintf("短枝损耗复核判花农责任（复核单 #%d，%s）", id, dispositionCN(in.Disposition)), linkedReturn); err != nil {
		fail(w, 500, err.Error())
		return
	}

	// 三种处置确认后统一进入「待花农回应」：花农可确认账扣/判责，或对损耗判定提出异议
	// （差价补偿冻结账扣金额；退货处置冻结关联退货扣款；下一批折扣无在途资金，仅冻结状态）
	reviewStatus := "confirmed"
	var reviewAckAt *time.Time

	if _, err := tx.ExecContext(r.Context(),
		`UPDATE short_stem_reviews
		 SET status=$9, confirmed_by=$1, confirmed_at=now(), acknowledged_at=$10,
		     observed_short_ratio=$2, disposition=$3, compensation_amount=$4,
		     next_discount_percent=$5, linked_return_id=$6, close_note=$7
		 WHERE id=$8`,
		u.ID, in.ObservedRatio, in.Disposition, compensation, in.NextDiscountPercent, linkedReturn,
		in.Note, id, reviewStatus, reviewAckAt); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 花农账扣记录：差价补偿记录补偿金额；退货/折扣金额为 0，账扣单仅承载花农确认/冻结状态
	// （退货款项走退货扣款口径，争议期间由复核状态冻结，见结算查询）
	ledgerStatus := "pending_ack"
	var lid int
	if err := tx.QueryRowContext(r.Context(),
		`INSERT INTO review_ledger (review_id, farmer_id, amount, status, confirmed_at)
		 VALUES ($1,$2,$3,$4, CASE WHEN $5='confirmed' THEN now() ELSE NULL END) RETURNING id`,
		id, farmerID, compensation, ledgerStatus, ledgerStatus).Scan(&lid); err != nil {
		fail(w, 500, err.Error())
		return
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
	writeJSON(w, 200, map[string]any{
		"id": id, "status": reviewStatus, "disposition": in.Disposition,
		"compensation_amount": compensation, "ledger_id": lid, "ledger_status": ledgerStatus,
		"linked_return_id": linkedReturn, "credit_delta": creditDelta,
	})
}

// deductFarmerCreditTx 花农责任扣分并留痕（退货/短枝复核复用，returnID 为 nil 时不关联退货单）
func deductFarmerCreditTx(tx *sql.Tx, farmerID int, delta int, reason string, returnID *int) error {
	if _, err := tx.Exec(
		`INSERT INTO farmer_credit_events (farmer_id, delta, reason, return_id) VALUES ($1,$2,$3,$4)`,
		farmerID, delta, reason, returnID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE users SET credit_score=GREATEST(0, credit_score+$1) WHERE id=$2`, delta, farmerID)
	return err
}

func dispositionCN(d string) string {
	return map[string]string{
		"price_compensation":  "差价补偿",
		"return_goods":        "退货",
		"next_batch_discount": "下一批折扣",
	}[d]
}

// handleDisputeReview 花农对损耗判定提出异议：两组证据封存保留，差价进入争议冻结
func (a *app) handleDisputeReview(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &in); err != nil || in.Reason == "" {
		fail(w, 400, "请填写异议理由")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var status string
	var farmerID int
	var ledgerID *int
	err = tx.QueryRowContext(r.Context(),
		`SELECT rv.status, rv.farmer_id, l.id FROM short_stem_reviews rv
		 LEFT JOIN review_ledger l ON l.review_id=rv.id
		 WHERE rv.id=$1 FOR UPDATE OF rv`, id).Scan(&status, &farmerID, &ledgerID)
	if err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if farmerID != u.ID {
		fail(w, 403, "只能对涉及本人的复核单提出异议")
		return
	}
	if status != "confirmed" {
		fail(w, 409, "仅质检已确认、待账扣的复核单可提出异议，当前: "+status)
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO review_disputes (review_id, farmer_id, reason) VALUES ($1,$2,$3)`,
		id, u.ID, in.Reason); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE short_stem_reviews SET status='disputed' WHERE id=$1`, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 财务在争议关闭前冻结相关差价：账扣进入冻结，结算暂不计入
	if ledgerID != nil {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE review_ledger SET status='frozen', frozen_at=COALESCE(frozen_at,now()) WHERE id=$1`,
			*ledgerID); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{"id": id, "status": "disputed", "ledger_status": "frozen"})
}

// handleFreezeReview 财务执行/确认冻结（争议中的账扣幂等冻结，留痕操作人）
func (a *app) handleFreezeReview(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	u := userOf(r)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var rvStatus, ledgerStatus string
	err = tx.QueryRowContext(r.Context(),
		`SELECT rv.status, COALESCE(l.status,'') FROM short_stem_reviews rv
		 LEFT JOIN review_ledger l ON l.review_id=rv.id WHERE rv.id=$1 FOR UPDATE OF rv`, id).
		Scan(&rvStatus, &ledgerStatus)
	if err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if rvStatus != "disputed" {
		fail(w, 409, "仅花农已提出异议的复核单可冻结，当前: "+rvStatus)
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE review_ledger SET status='frozen', frozen_by=$1, frozen_at=COALESCE(frozen_at,now())
		 WHERE review_id=$2`, u.ID, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "ledger_status": "frozen"})
}

// handleCloseReviewDispute 市场管理/质检关闭争议：维持原判定 / 调整差价 / 免除账扣
func (a *app) handleCloseReviewDispute(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Action string  `json:"action"` // uphold/adjust/waive
		Amount float64 `json:"amount"` // adjust 时的新差价
		Note   string  `json:"note"`
	}
	if err := decodeBody(r, &in); err != nil || !contains([]string{"uphold", "adjust", "waive"}, in.Action) {
		fail(w, 400, "action 须为 uphold/adjust/waive")
		return
	}
	if in.Action == "adjust" && in.Amount <= 0 {
		fail(w, 400, "调整后差价须大于 0（如需免除请用 waive）")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var status string
	var disputeID int
	var ledgerID sql.NullInt64
	err = tx.QueryRowContext(r.Context(),
		`SELECT rv.status, d.id, l.id
		 FROM short_stem_reviews rv
		 JOIN review_disputes d ON d.review_id=rv.id AND d.status='open'
		 LEFT JOIN review_ledger l ON l.review_id=rv.id
		 WHERE rv.id=$1 FOR UPDATE OF rv`, id).Scan(&status, &disputeID, &ledgerID)
	if err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在或没有进行中的争议")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "disputed" {
		fail(w, 409, "该复核单不在争议中: "+status)
		return
	}
	hasLedger := ledgerID.Valid
	lid := int(ledgerID.Int64)

	closeStatus := map[string]string{
		"uphold": "closed_uphold", "adjust": "closed_adjusted", "waive": "closed_waived",
	}[in.Action]
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE review_disputes SET status=$1, closed_by=$2, closed_at=now(), close_note=$3 WHERE id=$4`,
		closeStatus, u.ID, in.Note, disputeID); err != nil {
		fail(w, 500, err.Error())
		return
	}

	switch in.Action {
	case "waive":
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE short_stem_reviews SET status='waived', closed_by=$1, closed_at=now(), close_note=$2 WHERE id=$3`,
			u.ID, in.Note, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if hasLedger {
			if _, err := tx.ExecContext(r.Context(),
				`UPDATE review_ledger SET status='released' WHERE id=$1`, lid); err != nil {
				fail(w, 500, err.Error())
				return
			}
		}
	case "adjust":
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE short_stem_reviews SET status='confirmed', closed_by=$1, closed_at=now(),
			        compensation_amount=$2, close_note=$3 WHERE id=$4`,
			u.ID, round2(in.Amount), in.Note, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE review_ledger SET status='pending_ack', amount=$1 WHERE id=$2`, round2(in.Amount), lid); err != nil {
			fail(w, 500, err.Error())
			return
		}
	default: // uphold：解冻，回到待花农确认账扣
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE short_stem_reviews SET status='confirmed', closed_by=$1, closed_at=now(), close_note=$2 WHERE id=$3`,
			u.ID, in.Note, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE review_ledger SET status='pending_ack' WHERE id=$1`, lid); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "dispute_status": closeStatus, "action": in.Action})
}

// handleAcknowledgeReview 花农端确认账扣，账扣状态随即同步（结算时计入花农扣款）
func (a *app) handleAcknowledgeReview(w http.ResponseWriter, r *http.Request) {
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
	var status string
	var farmerID, ledgerID int
	var amount float64
	err = tx.QueryRowContext(r.Context(),
		`SELECT rv.status, rv.farmer_id, l.id, l.amount FROM short_stem_reviews rv
		 JOIN review_ledger l ON l.review_id=rv.id
		 WHERE rv.id=$1 FOR UPDATE OF rv`, id).Scan(&status, &farmerID, &ledgerID, &amount)
	if err == sql.ErrNoRows {
		fail(w, 404, "复核单不存在或无账扣记录")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if farmerID != u.ID {
		fail(w, 403, "只能确认本人的账扣")
		return
	}
	var ledgerStatus string
	if err := tx.QueryRowContext(r.Context(),
		`SELECT status FROM review_ledger WHERE id=$1 FOR UPDATE`, ledgerID).Scan(&ledgerStatus); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if ledgerStatus == "frozen" {
		fail(w, 409, "争议尚未关闭，差价冻结中，暂不能确认账扣")
		return
	}
	if ledgerStatus != "pending_ack" {
		fail(w, 409, "该账扣状态为 "+ledgerStatus+"，无需重复确认")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE review_ledger SET status='confirmed', confirmed_at=now() WHERE id=$1`, ledgerID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE short_stem_reviews SET status='acknowledged', acknowledged_at=now() WHERE id=$1
		 AND status='confirmed'`, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{
		"id": id, "review_status": "acknowledged", "ledger_status": "confirmed", "amount": amount,
	})
}

// handleListShortStemReviews 复核列表（花店/花农/批发商看自己相关；质检/财务/管理看全部）
func (a *app) handleListShortStemReviews(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT rv.id, COALESCE(rv.review_no,''), rv.order_id, COALESCE(o.order_no,''), rv.batch_id,
	             COALESCE(b.batch_no,''), b.category,
	             rv.shop_id, s.name, rv.wholesaler_id, w.name, rv.farmer_id, f.name,
	             rv.bundles, rv.declared_short_ratio, rv.observed_short_ratio, rv.price_per_bundle,
	             rv.status, rv.disposition, rv.compensation_amount, rv.next_discount_percent,
	             COALESCE(rv.linked_return_id,0), rv.shop_note, rv.close_note,
	             rv.created_at, rv.confirmed_at, rv.acknowledged_at, rv.closed_at,
	             COALESCE(l.id,0), COALESCE(l.amount,0), COALESCE(l.status,''),
	             l.frozen_at, l.confirmed_at
	      FROM short_stem_reviews rv
	      JOIN orders o ON o.id=rv.order_id
	      JOIN flower_batches b ON b.id=rv.batch_id
	      JOIN users s ON s.id=rv.shop_id
	      JOIN users w ON w.id=rv.wholesaler_id
	      JOIN users f ON f.id=rv.farmer_id
	      LEFT JOIN review_ledger l ON l.review_id=rv.id`
	args := []any{}
	switch u.Role {
	case "shop":
		q += " WHERE rv.shop_id=$1"
		args = append(args, u.ID)
	case "farmer":
		q += " WHERE rv.farmer_id=$1"
		args = append(args, u.ID)
	case "wholesaler":
		q += " WHERE rv.wholesaler_id=$1"
		args = append(args, u.ID)
	}
	if st := r.URL.Query().Get("status"); st != "" {
		if len(args) == 0 {
			q += " WHERE rv.status=$1"
		} else {
			q += fmt.Sprintf(" AND rv.status=$%d", len(args)+1)
		}
		args = append(args, st)
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY rv.id DESC LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	ids := []int{}
	rowMaps := []map[string]any{}
	for rows.Next() {
		var (
			id, orderID, batchID, shopID, wholesalerID, farmerID, bundles, returnID, ledgerID int
			reviewNo, orderNo, batchNo, cat, shop, whole, farmer, status, disposition         string
			shopNote, closeNote, ledgerStatus                                                 string
			declared, observed, price, comp, discount, ledgerAmount                           float64
			createdAt                                                                         time.Time
			confirmedAt, acknowledgedAt, closedAt, frozenAt, ledgerConfirmedAt                *time.Time
		)
		if rows.Scan(&id, &reviewNo, &orderID, &orderNo, &batchID, &batchNo, &cat,
			&shopID, &shop, &wholesalerID, &whole, &farmerID, &farmer,
			&bundles, &declared, &observed, &price,
			&status, &disposition, &comp, &discount, &returnID, &shopNote, &closeNote,
			&createdAt, &confirmedAt, &acknowledgedAt, &closedAt,
			&ledgerID, &ledgerAmount, &ledgerStatus, &frozenAt, &ledgerConfirmedAt) != nil {
			continue
		}
		m := map[string]any{
			"id": id, "review_no": reviewNo, "order_id": orderID, "order_no": orderNo,
			"batch_id": batchID, "batch_no": batchNo, "category": cat,
			"shop_id": shopID, "shop_name": shop,
			"wholesaler_id": wholesalerID, "wholesaler_name": whole,
			"farmer_id": farmerID, "farmer_name": farmer,
			"bundles": bundles, "declared_short_ratio": declared, "observed_short_ratio": observed,
			"price_per_bundle": price, "status": status, "disposition": disposition,
			"compensation_amount": comp, "next_discount_percent": discount,
			"linked_return_id": returnID, "shop_note": shopNote, "close_note": closeNote,
			"created_at": createdAt, "confirmed_at": confirmedAt, "acknowledged_at": acknowledgedAt,
			"closed_at": closedAt,
			"ledger": map[string]any{
				"id": ledgerID, "amount": ledgerAmount, "status": ledgerStatus,
				"frozen_at": frozenAt, "confirmed_at": ledgerConfirmedAt,
			},
		}
		out = append(out, m)
		rowMaps = append(rowMaps, m)
		ids = append(ids, id)
	}
	rows.Close()

	// 组装两组证据与争议（小表 N+1 可接受，复核单量低）
	evByReview := map[int][]map[string]any{}
	if len(ids) > 0 {
		erows, err := a.db.QueryContext(r.Context(),
			`SELECT review_id, id, group_name, evidence_type, photo_url, detail, created_at FROM review_evidences
			 WHERE review_id = ANY($1) ORDER BY id`, ids)
		if err == nil {
			for erows.Next() {
				var rid, eid int
				var group, typ, url, detail string
				var at time.Time
				if erows.Scan(&rid, &eid, &group, &typ, &url, &detail, &at) == nil {
					evByReview[rid] = append(evByReview[rid], map[string]any{
						"id": eid, "group": group, "type": typ, "photo_url": url, "detail": detail, "created_at": at,
					})
				}
			}
			erows.Close()
		}
	}
	dispByReview := map[int]map[string]any{}
	if len(ids) > 0 {
		drows, err := a.db.QueryContext(r.Context(),
			`SELECT d.review_id, d.id, d.reason, d.status, d.close_note, d.created_at, d.closed_at, u.name
			 FROM review_disputes d JOIN users u ON u.id=d.farmer_id
			 WHERE d.review_id = ANY($1) ORDER BY d.id DESC`, ids)
		if err == nil {
			for drows.Next() {
				var rid, did int
				var reason, status, note, farmer string
				var at time.Time
				var closedAt *time.Time
				if drows.Scan(&rid, &did, &reason, &status, &note, &at, &closedAt, &farmer) == nil {
					if _, ok := dispByReview[rid]; !ok {
						dispByReview[rid] = map[string]any{
							"id": did, "reason": reason, "status": status, "close_note": note,
							"created_at": at, "closed_at": closedAt, "farmer_name": farmer,
						}
					}
				}
			}
			drows.Close()
		}
	}
	for i, id := range ids {
		if evs, ok := evByReview[id]; ok {
			rowMaps[i]["evidences"] = evs
		} else {
			rowMaps[i]["evidences"] = []map[string]any{}
		}
		if d, ok := dispByReview[id]; ok {
			rowMaps[i]["dispute"] = d
		} else {
			rowMaps[i]["dispute"] = nil
		}
	}
	writeJSON(w, 200, out)
}
