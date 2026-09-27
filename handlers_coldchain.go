package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// querier 抽象 *sql.DB 与 *sql.Tx，便于调度队列在事务内外复用
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// slotFullError 冷库库位不足（携带剩余量信息，409 时附带调度队列）
type slotFullError struct{ msg string }

func (e *slotFullError) Error() string { return e.msg }

// ---------- 冷库库位不足调度 ----------

// dispatchQueue 计算爆满时的优先入库队列：
// 排序 = 花材敏感度(高→低) → 已成交状态(已成交→未成交) → 预计提货时间(早→晚)
func dispatchQueue(ctx context.Context, q querier) ([]map[string]any, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT b.id, b.batch_no, b.category, b.origin, b.grade, b.bundles, b.sensitivity, b.status,
		       b.expected_pickup_at, h.id, h.method, h.status
		FROM flower_batches b
		LEFT JOIN LATERAL (
		  SELECT id, method, status FROM holding_records
		  WHERE batch_id=b.id AND status IN ('active','failed') ORDER BY id DESC LIMIT 1
		) h ON true
		WHERE b.storage_slot_id IS NULL
		  AND b.status IN ('qc_passed','qc_flagged','in_auction','sold')
		ORDER BY CASE b.sensitivity WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END,
		         CASE WHEN b.status='sold' THEN 0 ELSE 1 END,
		         b.expected_pickup_at ASC NULLS LAST,
		         b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	rank := 0
	for rows.Next() {
		var (
			id, bundles                       int
			batchNo, cat, origin, grade, sens string
			status                            string
			pickup                            *time.Time
			holdingID                         *int
			holdingMethod, holdingStatus      *string
		)
		if err := rows.Scan(&id, &batchNo, &cat, &origin, &grade, &bundles, &sens, &status,
			&pickup, &holdingID, &holdingMethod, &holdingStatus); err != nil {
			continue
		}
		rank++
		sold := status == "sold"
		reason := map[string]string{"high": "高敏感", "medium": "中敏感", "low": "低敏感"}[sens]
		if sold {
			reason += "·已成交"
		}
		if pickup != nil {
			reason += "·提货 " + pickup.Format("15:04")
		} else {
			reason += "·未登记提货时间"
		}
		out = append(out, map[string]any{
			"rank": rank, "batch_id": id, "batch_no": batchNo, "category": cat, "origin": origin,
			"grade": grade, "bundles": bundles, "sensitivity": sens, "status": status,
			"sold": sold, "expected_pickup_at": pickup, "reason": reason,
			"holding_id": holdingID, "holding_method": holdingMethod, "holding_status": holdingStatus,
		})
	}
	return out, nil
}

func freeBundles(ctx context.Context, q querier) int {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(capacity_bundles-occupied_bundles),0) FROM storage_slots`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// handleDispatchQueue 查看冷库爆满时的优先入库调度队列（全角色可读）
func (a *app) handleDispatchQueue(w http.ResponseWriter, r *http.Request) {
	queue, err := dispatchQueue(r.Context(), a.db)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"free_bundles": freeBundles(r.Context(), a.db),
		"rule":         "优先入库顺序 = 花材敏感度(高→低) → 已成交(先) → 预计提货时间(早→晚)",
		"queue":        queue,
	})
}

// storeBatchTx 批次入库核心逻辑（冷库直接入库 / 保温转入库共用）
func storeBatchTx(ctx context.Context, tx *sql.Tx, batchID, slotID int) (string, error) {
	var status string
	var bundles int
	var slotIDPtr *int
	err := tx.QueryRowContext(ctx,
		`SELECT status, bundles, storage_slot_id FROM flower_batches WHERE id=$1 FOR UPDATE`, batchID).
		Scan(&status, &bundles, &slotIDPtr)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("批次不存在")
	} else if err != nil {
		return "", err
	}
	if slotIDPtr != nil {
		return "", fmt.Errorf("该批次已入库")
	}
	// 已成交(sold)/竞价中(in_auction) 但尚未入库的批次同样允许入库（已成交状态优先）
	if status != "qc_passed" && status != "qc_flagged" && status != "in_auction" && status != "sold" {
		return "", fmt.Errorf("仅质检后的批次可入库，当前状态: %s", status)
	}
	var code string
	var capacity, occupied int
	err = tx.QueryRowContext(ctx,
		`SELECT code, capacity_bundles, occupied_bundles FROM storage_slots WHERE id=$1 FOR UPDATE`, slotID).
		Scan(&code, &capacity, &occupied)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("库位不存在")
	} else if err != nil {
		return "", err
	}
	if occupied+bundles > capacity {
		return "", &slotFullError{msg: fmt.Sprintf(
			"冷库库位不足：库位 %s 剩余 %d 扎，本批次需要 %d 扎。凌晨到货高峰请按调度队列优先入库，未入库批次请登记临时保温",
			code, capacity-occupied, bundles)}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE storage_slots SET occupied_bundles=occupied_bundles+$1 WHERE id=$2`, bundles, slotID); err != nil {
		return "", err
	}
	newStatus := "stored"
	if status == "in_auction" || status == "sold" {
		newStatus = status // 已成交/竞价中批次入库不改变其交易状态
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE flower_batches SET storage_slot_id=$1, stored_at=now(), status=$2 WHERE id=$3`, slotID, newStatus, batchID); err != nil {
		return "", err
	}
	// 该批次若有未完结的临时保温记录，转入库后自动完结
	if _, err := tx.ExecContext(ctx,
		`UPDATE holding_records SET status='stored' WHERE batch_id=$1 AND status IN ('active','failed')`, batchID); err != nil {
		return "", err
	}
	return code, nil
}

// ---------- 临时保温（未入库花材） ----------

// handleCreateHolding 冷库登记未入库批次的临时保温方式与损耗责任
func (a *app) handleCreateHolding(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		BatchID        int    `json:"batch_id"`
		Method         string `json:"method"`
		Responsibility string `json:"responsibility"`
		Note           string `json:"note"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if !contains(holdingMethods, in.Method) {
		fail(w, 400, "保温方式须为 ice_box冰袋保温箱/refrigerated_truck冷藏车/insulated_cover保温被/cool_room临租小冷库/other")
		return
	}
	if !contains(responsibilityParties, in.Responsibility) {
		fail(w, 400, "损耗责任方须为 coldstore冷库/wholesaler批发商/farmer花农/broker经纪人")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var status string
	var slotID *int
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, storage_slot_id FROM flower_batches WHERE id=$1 FOR UPDATE`, in.BatchID).
		Scan(&status, &slotID)
	if err == sql.ErrNoRows {
		fail(w, 404, "批次不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if slotID != nil {
		fail(w, 409, "该批次已入库，无需临时保温")
		return
	}
	if status != "qc_passed" && status != "qc_flagged" && status != "in_auction" && status != "sold" {
		fail(w, 409, "仅质检后的批次可登记临时保温，当前状态: "+status)
		return
	}
	var dup int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT count(*) FROM holding_records WHERE batch_id=$1 AND status IN ('active','failed')`, in.BatchID).Scan(&dup); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if dup > 0 {
		fail(w, 409, "该批次已有未完结的临时保温记录")
		return
	}
	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO holding_records (batch_id, method, responsibility, note, created_by)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`, in.BatchID, in.Method, in.Responsibility, in.Note, u.ID).Scan(&id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{"id": id, "batch_id": in.BatchID, "status": "active"})
}

const holdingSelect = `
 SELECT h.id, h.batch_id, b.batch_no, b.category, b.origin, b.bundles, b.sensitivity, b.expected_pickup_at,
        h.method, h.responsibility, h.note, h.status, h.failure_note, h.failed_at,
        h.coldstore_confirmed_by, h.coldstore_confirmed_at, h.coldstore_opinion,
        h.wholesaler_confirmed_by, h.wholesaler_confirmed_at, h.wholesaler_opinion,
        h.final_responsibility, h.compensation, h.created_at, cu.name,
        COALESCE(i.wholesaler_id,0), COALESCE(wu.name,'')
 FROM holding_records h
 JOIN flower_batches b ON b.id=h.batch_id
 JOIN users cu ON cu.id=h.created_by
 LEFT JOIN inventories i ON i.batch_id=b.id
 LEFT JOIN users wu ON wu.id=i.wholesaler_id`

func scanHolding(rows *sql.Rows) (map[string]any, error) {
	var (
		id, batchID, bundles                               int
		batchNo, cat, origin, sens, method, resp, note     string
		status, failNote, csOp, wsOp, finalResp, createdBy string
		compensation                                       float64
		pickup, failedAt, csAt, wsAt                       *time.Time
		csBy, wsBy                                         *int
		createdAt                                          time.Time
		wholesalerID                                       int
		wholesalerName                                     string
	)
	err := rows.Scan(&id, &batchID, &batchNo, &cat, &origin, &bundles, &sens, &pickup,
		&method, &resp, &note, &status, &failNote, &failedAt,
		&csBy, &csAt, &csOp, &wsBy, &wsAt, &wsOp,
		&finalResp, &compensation, &createdAt, &createdBy, &wholesalerID, &wholesalerName)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "batch_id": batchID, "batch_no": batchNo, "category": cat, "origin": origin,
		"bundles": bundles, "sensitivity": sens, "expected_pickup_at": pickup,
		"method": method, "responsibility": resp, "note": note, "status": status,
		"failure_note": failNote, "failed_at": failedAt,
		"coldstore_confirmed_by": csBy, "coldstore_confirmed_at": csAt, "coldstore_opinion": csOp,
		"wholesaler_confirmed_by": wsBy, "wholesaler_confirmed_at": wsAt, "wholesaler_opinion": wsOp,
		"final_responsibility": finalResp, "compensation": compensation,
		"created_at": createdAt, "created_by": createdBy,
		"wholesaler_id": wholesalerID, "wholesaler_name": wholesalerName,
	}, nil
}

// handleListHoldings 临时保温记录（批发商看与自己相关的，花农/经纪人看自己批次的，其余看全部）
func (a *app) handleListHoldings(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := holdingSelect
	args := []any{}
	switch u.Role {
	case "wholesaler":
		q += ` WHERE (i.wholesaler_id=$1 OR h.wholesaler_confirmed_by=$1
		         OR (h.status='failed' AND h.wholesaler_confirmed_by IS NULL))`
		args = append(args, u.ID)
	case "farmer":
		q += ` WHERE b.farmer_id=$1`
		args = append(args, u.ID)
	case "broker":
		q += ` WHERE b.broker_id=$1`
		args = append(args, u.ID)
	}
	if st := r.URL.Query().Get("status"); st != "" {
		if len(args) == 0 {
			q += " WHERE h.status=$1"
		} else {
			q += fmt.Sprintf(" AND h.status=$%d", len(args)+1)
		}
		args = append(args, st)
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY h.id DESC LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		if m, err := scanHolding(rows); err == nil {
			out = append(out, m)
		}
	}
	writeJSON(w, 200, out)
}

// handleHoldingFail 冷库上报保温失败（花材开放过快），进入双方确认责任流程
func (a *app) handleHoldingFail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		FailureNote  string  `json:"failure_note"`
		Compensation float64 `json:"compensation"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.FailureNote == "" {
		fail(w, 400, "请填写保温失败说明（如：冰袋耗尽，花材开放过快）")
		return
	}
	if in.Compensation < 0 {
		fail(w, 400, "赔付金额不能为负")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var status, batchNo string
	var batchID int
	err = tx.QueryRowContext(r.Context(),
		`SELECT h.status, h.batch_id, b.batch_no FROM holding_records h
		 JOIN flower_batches b ON b.id=h.batch_id WHERE h.id=$1 FOR UPDATE`, id).
		Scan(&status, &batchID, &batchNo)
	if err == sql.ErrNoRows {
		fail(w, 404, "保温记录不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "active" {
		fail(w, 409, "仅保温中的记录可上报失败，当前状态: "+status)
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE holding_records SET status='failed', failure_note=$1, failed_at=now(), compensation=$2 WHERE id=$3`,
		in.FailureNote, in.Compensation, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 花材开放过快 → 风险标记 + 可售时长缩短 1/4
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE flower_batches SET
		   risk_flags = CASE WHEN risk_flags='' THEN 'over_opened'
		                     WHEN position('over_opened' in risk_flags)>0 THEN risk_flags
		                     ELSE risk_flags||',over_opened' END,
		   sellable_hours = GREATEST(4, (sellable_hours*3/4)::int)
		 WHERE id=$1`, batchID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 通知冷库负责人与相关批发商双方确认责任；同步市场管理方
	title := fmt.Sprintf("临时保温失败：批次 %s 花材开放过快", batchNo)
	body := fmt.Sprintf("失败说明：%s。请冷库负责人与批发商双方确认损耗责任。", in.FailureNote)
	if err := notifyTx(r.Context(), tx, nil, "coldstore", "holding_failed", id, title, body); err != nil {
		fail(w, 500, err.Error())
		return
	}
	var winnerID *int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT wholesaler_id FROM inventories WHERE batch_id=$1`, batchID).Scan(&winnerID); err != nil && err != sql.ErrNoRows {
		fail(w, 500, err.Error())
		return
	}
	if winnerID != nil {
		if err := notifyTx(r.Context(), tx, winnerID, "wholesaler", "holding_failed", id, title, body); err != nil {
			fail(w, 500, err.Error())
			return
		}
	} else {
		if err := notifyTx(r.Context(), tx, nil, "wholesaler", "holding_failed", id, title, body); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := notifyTx(r.Context(), tx, nil, "manager", "holding_failed", id, title, body); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": "failed",
		"next": "等待冷库负责人与批发商双方确认责任（双方认定一致后定责）"})
}

// handleHoldingConfirm 冷库负责人 / 批发商 各自确认损耗责任（双方一致才定责）
func (a *app) handleHoldingConfirm(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Responsibility string `json:"responsibility"` // 本方认定的责任方
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if !contains(responsibilityParties, in.Responsibility) {
		fail(w, 400, "责任方须为 coldstore冷库/wholesaler批发商/farmer花农/broker经纪人")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var (
		status, batchNo string
		batchID         int
		csBy, wsBy      *int
		csOp, wsOp      string
		winnerID        *int
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT h.status, h.batch_id, b.batch_no, h.coldstore_confirmed_by, h.wholesaler_confirmed_by,
		        h.coldstore_opinion, h.wholesaler_opinion,
		        (SELECT wholesaler_id FROM inventories WHERE batch_id=h.batch_id)
		 FROM holding_records h JOIN flower_batches b ON b.id=h.batch_id
		 WHERE h.id=$1 FOR UPDATE`, id).
		Scan(&status, &batchID, &batchNo, &csBy, &wsBy, &csOp, &wsOp, &winnerID)
	if err == sql.ErrNoRows {
		fail(w, 404, "保温记录不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "failed" {
		fail(w, 409, "仅保温失败的记录需要确认责任，当前状态: "+status)
		return
	}
	side := ""
	switch u.Role {
	case "coldstore", "manager":
		if csBy != nil {
			fail(w, 409, "冷库方已确认过责任")
			return
		}
		side = "coldstore"
	case "wholesaler":
		if wsBy != nil {
			fail(w, 409, "批发方已确认过责任")
			return
		}
		// 已成交批次须由买入的批发商本人确认
		if winnerID != nil && *winnerID != u.ID {
			fail(w, 403, "该批次已由其他批发商成交，须由买入批发商确认")
			return
		}
		side = "wholesaler"
	default:
		fail(w, 403, "仅冷库负责人或批发商可确认责任")
		return
	}
	if side == "coldstore" {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE holding_records SET coldstore_confirmed_by=$1, coldstore_confirmed_at=now(), coldstore_opinion=$2 WHERE id=$3`,
			u.ID, in.Responsibility, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		csOp = in.Responsibility
		csBy = &u.ID
	} else {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE holding_records SET wholesaler_confirmed_by=$1, wholesaler_confirmed_at=now(), wholesaler_opinion=$2 WHERE id=$3`,
			u.ID, in.Responsibility, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		wsOp = in.Responsibility
		wsBy = &u.ID
	}
	final := ""
	if csBy != nil && wsBy != nil {
		if csOp == wsOp {
			final = csOp
		} else {
			final = "disputed" // 双方认定不一致，留痕待市场协调
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE holding_records SET final_responsibility=$1 WHERE id=$2`, final, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		// 双签完成 → 通知市场管理方，后续调整该时段入场顺序
		title := fmt.Sprintf("保温失败责任已双签：批次 %s", batchNo)
		body := fmt.Sprintf("冷库认定「%s」，批发方认定「%s」，最终：%s。请评估调整该时段入场顺序。",
			csOp, wsOp, map[string]string{"disputed": "存在分歧"}[final])
		if final != "disputed" {
			body = fmt.Sprintf("双方一致认定责任方为「%s」。请评估调整该时段入场顺序。", final)
		}
		if err := notifyTx(r.Context(), tx, nil, "manager", "holding_confirmed", id, title, body); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "side": side, "final_responsibility": final})
}

// handleHoldingStore 保温批次转入库（库位释放后）
func (a *app) handleHoldingStore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		SlotID int `json:"slot_id"`
	}
	if err := decodeBody(r, &in); err != nil || in.SlotID == 0 {
		fail(w, 400, "请指定 slot_id")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var status string
	var batchID int
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, batch_id FROM holding_records WHERE id=$1 FOR UPDATE`, id).Scan(&status, &batchID)
	if err == sql.ErrNoRows {
		fail(w, 404, "保温记录不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "active" && status != "failed" {
		fail(w, 409, "该保温记录已完结，当前状态: "+status)
		return
	}
	code, err := storeBatchTx(r.Context(), tx, batchID, in.SlotID)
	if err != nil {
		if se, ok2 := err.(*slotFullError); ok2 {
			queue, _ := dispatchQueue(r.Context(), tx)
			writeJSON(w, 409, map[string]any{"error": se.msg,
				"free_bundles": freeBundles(r.Context(), tx), "queue": queue})
			return
		}
		fail(w, 409, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "batch_id": batchID, "status": "stored", "slot_code": code})
}

// ---------- 冷库端复盘 ----------

// handleCreateReview 冷库端对保温失败事件同步复盘（根因 + 改进措施）
func (a *app) handleCreateReview(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		RootCause   string `json:"root_cause"`
		Improvement string `json:"improvement"`
	}
	if err := decodeBody(r, &in); err != nil || in.RootCause == "" || in.Improvement == "" {
		fail(w, 400, "请填写根因分析与改进措施")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var status, batchNo string
	err = tx.QueryRowContext(r.Context(),
		`SELECT h.status, b.batch_no FROM holding_records h JOIN flower_batches b ON b.id=h.batch_id
		 WHERE h.id=$1 FOR UPDATE`, id).Scan(&status, &batchNo)
	if err == sql.ErrNoRows {
		fail(w, 404, "保温记录不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "failed" {
		fail(w, 409, "仅保温失败的记录可复盘，当前状态: "+status)
		return
	}
	var rid int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO cold_store_reviews (holding_id, reviewer_id, root_cause, improvement)
		 VALUES ($1,$2,$3,$4) RETURNING id`, id, u.ID, in.RootCause, in.Improvement).Scan(&rid)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 复盘完成 → 提醒市场管理方可据此调整该时段入场顺序
	if err := notifyTx(r.Context(), tx, nil, "manager", "review_done", rid,
		fmt.Sprintf("冷库已复盘：批次 %s 保温失败", batchNo),
		fmt.Sprintf("根因：%s；改进：%s。可发起该时段入场顺序调整。", in.RootCause, in.Improvement)); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": rid, "holding_id": id})
}

// handleListReviews 冷库复盘记录（全角色可读，冷库端同步）
func (a *app) handleListReviews(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT r.id, r.holding_id, b.batch_no, r.root_cause, r.improvement, r.created_at, u.name
		 FROM cold_store_reviews r
		 JOIN holding_records h ON h.id=r.holding_id
		 JOIN flower_batches b ON b.id=h.batch_id
		 JOIN users u ON u.id=r.reviewer_id
		 ORDER BY r.id DESC LIMIT 100`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, holdingID int
		var batchNo, cause, improvement, reviewer string
		var at time.Time
		if rows.Scan(&id, &holdingID, &batchNo, &cause, &improvement, &at, &reviewer) == nil {
			out = append(out, map[string]any{"id": id, "holding_id": holdingID, "batch_no": batchNo,
				"root_cause": cause, "improvement": improvement, "created_at": at, "reviewer": reviewer})
		}
	}
	writeJSON(w, 200, out)
}

// ---------- 入场顺序调整（市场 → 通知经纪人 → 经纪人确认） ----------

// handleCreateEntryAdjustment 市场管理方调整某时段入场顺序，并通知经纪人
func (a *app) handleCreateEntryAdjustment(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		TimeSlot  string `json:"time_slot"`
		HoldingID *int   `json:"holding_id"`
		Reason    string `json:"reason"`
		Detail    string `json:"detail"`
		BrokerID  *int   `json:"broker_id"` // 指定需确认的经纪人，空=全体经纪人
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if !contains(arrivalTimeSlots, in.TimeSlot) {
		fail(w, 400, "时段须为 00:00-02:00/02:00-04:00/04:00-06:00/06:00-08:00")
		return
	}
	if in.Reason == "" || in.Detail == "" {
		fail(w, 400, "请填写调整原因与调整后的入场顺序说明")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	if in.HoldingID != nil {
		var n int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT count(*) FROM holding_records WHERE id=$1`, *in.HoldingID).Scan(&n); err != nil || n == 0 {
			fail(w, 400, "关联的保温记录不存在")
			return
		}
	}
	if in.BrokerID != nil {
		var role string
		if err := tx.QueryRowContext(r.Context(),
			`SELECT role FROM users WHERE id=$1`, *in.BrokerID).Scan(&role); err != nil || role != "broker" {
			fail(w, 400, "指定的经纪人不存在")
			return
		}
	}
	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO entry_adjustments (time_slot, holding_id, reason, detail, created_by, broker_id)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		in.TimeSlot, in.HoldingID, in.Reason, in.Detail, u.ID, in.BrokerID).Scan(&id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 通知经纪人确认
	title := fmt.Sprintf("入场顺序调整（%s 时段）", in.TimeSlot)
	body := fmt.Sprintf("原因：%s。调整后：%s。请经纪人确认。", in.Reason, in.Detail)
	if err := notifyTx(r.Context(), tx, in.BrokerID, "broker", "entry_adjustment", id, title, body); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "status": "pending", "time_slot": in.TimeSlot})
}

// handleListEntryAdjustments 入场调整列表（经纪人只看与自己相关的）
func (a *app) handleListEntryAdjustments(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT e.id, e.time_slot, e.holding_id, e.reason, e.detail, e.status,
	             e.created_at, cu.name, e.broker_id, COALESCE(bu.name,''),
	             e.confirmed_by, COALESCE(cf.name,''), e.confirmed_at
	      FROM entry_adjustments e
	      JOIN users cu ON cu.id=e.created_by
	      LEFT JOIN users bu ON bu.id=e.broker_id
	      LEFT JOIN users cf ON cf.id=e.confirmed_by`
	args := []any{}
	if u.Role == "broker" {
		q += ` WHERE (e.broker_id=$1 OR e.broker_id IS NULL)`
		args = append(args, u.ID)
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY e.id DESC LIMIT 100", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id                        int
			slot, reason, detail      string
			status, createdBy, broker string
			holdingID                 *int
			brokerID                  *int
			confirmedBy               *int
			confirmedName             string
			createdAt                 time.Time
			confirmedAt               *time.Time
		)
		if rows.Scan(&id, &slot, &holdingID, &reason, &detail, &status,
			&createdAt, &createdBy, &brokerID, &broker, &confirmedBy, &confirmedName, &confirmedAt) == nil {
			out = append(out, map[string]any{
				"id": id, "time_slot": slot, "holding_id": holdingID, "reason": reason, "detail": detail,
				"status": status, "created_at": createdAt, "created_by": createdBy,
				"broker_id": brokerID, "broker_name": broker,
				"confirmed_by": confirmedBy, "confirmed_name": confirmedName, "confirmed_at": confirmedAt,
			})
		}
	}
	writeJSON(w, 200, out)
}

// handleConfirmEntryAdjustment 经纪人确认入场顺序调整
func (a *app) handleConfirmEntryAdjustment(w http.ResponseWriter, r *http.Request) {
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
	var brokerID *int
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, broker_id FROM entry_adjustments WHERE id=$1 FOR UPDATE`, id).Scan(&status, &brokerID)
	if err == sql.ErrNoRows {
		fail(w, 404, "入场调整不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "pending" {
		fail(w, 409, "该调整已确认")
		return
	}
	if brokerID != nil && *brokerID != u.ID {
		fail(w, 403, "该调整指定由其他经纪人确认")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE entry_adjustments SET status='confirmed', confirmed_by=$1, confirmed_at=now() WHERE id=$2`,
		u.ID, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": "confirmed"})
}

// ---------- 站内通知 ----------

// notifyTx 写站内通知：指定 userID 发单人，否则按角色逐人发
func notifyTx(ctx context.Context, tx *sql.Tx, userID *int, role, kind string, refID int, title, body string) error {
	if userID != nil {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO notifications (user_id, role, kind, ref_id, title, body) VALUES ($1,$2,$3,$4,$5,$6)`,
			*userID, role, kind, refID, title, body)
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO notifications (user_id, role, kind, ref_id, title, body)
		 SELECT id, $1, $2, $3, $4, $5 FROM users WHERE role=$1`, role, kind, refID, title, body)
	return err
}

// handleListNotifications 我的通知（未读在前）
func (a *app) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, kind, ref_id, title, body, read_at, created_at
		 FROM notifications WHERE user_id=$1
		 ORDER BY (read_at IS NULL) DESC, id DESC LIMIT 100`, u.ID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	unread := 0
	for rows.Next() {
		var id, refID int
		var kind, title, body string
		var readAt *time.Time
		var createdAt time.Time
		if rows.Scan(&id, &kind, &refID, &title, &body, &readAt, &createdAt) == nil {
			if readAt == nil {
				unread++
			}
			out = append(out, map[string]any{"id": id, "kind": kind, "ref_id": refID, "title": title,
				"body": body, "read_at": readAt, "created_at": createdAt})
		}
	}
	writeJSON(w, 200, map[string]any{"unread": unread, "items": out})
}

// handleReadNotification 标记通知已读
func (a *app) handleReadNotification(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	if _, err := a.db.ExecContext(r.Context(),
		`UPDATE notifications SET read_at=now() WHERE id=$1 AND user_id=$2 AND read_at IS NULL`, id, u.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "read": true})
}
