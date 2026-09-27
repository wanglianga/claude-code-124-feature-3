package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// 凌晨到货高峰冷库爆满时的库位不足调度：
// 按 花材敏感度 → 已成交状态 → 预计提货时间 决定优先入库；
// 未入库花材登记临时保温方式与损耗责任预案；
// 临时保温失败（开放过快）由冷库负责人与批发商双方确认责任，冷库端同步复盘；
// 复盘后市场调整该时段入场顺序并通知经纪人，经纪人确认。

var tempMethods = []string{"ice_pack", "quilt", "ventilated", "shadow", "fan", "other"}
var lossRespOptions = []string{"farmer", "coldstore", "wholesaler", "shared"}

var tempMethodCN = map[string]string{
	"ice_pack": "加冰瓶/冰袋", "quilt": "保温棉被", "ventilated": "通风暂存区",
	"shadow": "遮阳阴凉处", "fan": "风机降温", "other": "其他临时措施",
}
var lossRespCN = map[string]string{
	"farmer": "花农承担", "coldstore": "冷库承担", "wholesaler": "批发商承担", "shared": "共同承担",
}

// defaultSensitivity 缺省花材敏感度（1-5，越大越敏感、越优先入库）
var defaultSensitivity = map[string]int{
	"rose":        5, // 玫瑰最易开放过快
	"eustoma":     4, // 洋桔梗
	"tulip":       4, // 郁金香
	"lily":        3, // 百合
	"holiday_mix": 3, // 节日混搭
	"eucalyptus":  1, // 尤加利耐放
}

// coldPriorityScore 调度优先级快照分（实际排序以 SQL 实时规则为准，此值用于展示）
// 敏感度（百万位） > 已成交（十万位） > 预计提货时间（万位内，越早越大）
func coldPriorityScore(sensitivity int, isSold bool, eta *time.Time) float64 {
	score := float64(sensitivity) * 1_000_000
	if isSold {
		score += 100_000
	}
	if eta != nil {
		anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		h := eta.UTC().Sub(anchor).Hours()
		if h < 0 {
			h = 0
		}
		if h > 99_999 {
			h = 99_999
		}
		score += 100_000 - h
	}
	return round2(score)
}

// notify 写站内通知
func (a *app) notify(ctx context.Context, userID int, typ, title, body, refType string, refID int, needAck bool) {
	a.db.ExecContext(ctx,
		`INSERT INTO notifications (user_id, type, title, body, ref_type, ref_id, acked)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`, userID, typ, title, body, refType, refID, needAck)
}

// notifyRole 给某角色全部用户发通知
func (a *app) notifyRole(ctx context.Context, role, typ, title, body, refType string, refID int, needAck bool) {
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM users WHERE role=$1`, role)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		if rows.Scan(&id) == nil {
			a.notify(ctx, id, typ, title, body, refType, refID, needAck)
		}
	}
}

// ---------- 花材敏感度配置 ----------

func (a *app) handleListSensitivity(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT category, sensitivity, temp_window, updated_at FROM category_sensitivity ORDER BY sensitivity DESC`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	found := map[string]bool{}
	out := []map[string]any{}
	for rows.Next() {
		var cat, tw string
		var sens int
		var at time.Time
		if rows.Scan(&cat, &sens, &tw, &at) == nil {
			found[cat] = true
			out = append(out, map[string]any{"category": cat, "category_cn": catCN(cat),
				"sensitivity": sens, "temp_window": tw, "updated_at": at})
		}
	}
	// 补齐未配置品类的缺省值
	for _, c := range categories {
		if !found[c] {
			out = append(out, map[string]any{"category": c, "category_cn": catCN(c),
				"sensitivity": defaultSensitivity[c], "temp_window": "", "updated_at": nil})
		}
	}
	writeJSON(w, 200, out)
}

func catCN(c string) string {
	if n, ok := map[string]string{
		"rose": "玫瑰", "lily": "百合", "tulip": "郁金香", "eustoma": "洋桔梗",
		"eucalyptus": "尤加利", "holiday_mix": "节日混搭",
	}[c]; ok {
		return n
	}
	return c
}

// handleUpsertSensitivity 市场管理方调整花材敏感度（决定库位不足时的优先入库顺序）
func (a *app) handleUpsertSensitivity(w http.ResponseWriter, r *http.Request) {
	cat := r.PathValue("category")
	if !contains(categories, cat) {
		fail(w, 400, "品类须为 rose/lily/tulip/eustoma/eucalyptus/holiday_mix")
		return
	}
	var in struct {
		Sensitivity int    `json:"sensitivity"`
		TempWindow  string `json:"temp_window"`
	}
	if err := decodeBody(r, &in); err != nil || in.Sensitivity < 1 || in.Sensitivity > 5 {
		fail(w, 400, "敏感度须为 1-5（越大越优先入库）")
		return
	}
	if _, err := a.db.ExecContext(r.Context(),
		`INSERT INTO category_sensitivity (category, sensitivity, temp_window, updated_at)
		 VALUES ($1,$2,$3,now())
		 ON CONFLICT (category) DO UPDATE SET sensitivity=$2, temp_window=$3, updated_at=now()`,
		cat, in.Sensitivity, in.TempWindow); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"category": cat, "sensitivity": in.Sensitivity, "temp_window": in.TempWindow})
}

// sensitivityOf 取某品类敏感度（无配置用缺省）
func (a *app) sensitivityOf(ctx context.Context, category string) int {
	var s int
	if err := a.db.QueryRowContext(ctx, `SELECT sensitivity FROM category_sensitivity WHERE category=$1`, category).Scan(&s); err == nil {
		return s
	}
	return defaultSensitivity[category]
}

// ---------- 等待入库队列（临时保温登记） ----------

// handleEnqueue 冷库爆满时为质检通过但无法入库的批次登记排队与临时保温方式
func (a *app) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		BatchID            int    `json:"batch_id"`
		PickupETA          string `json:"pickup_eta"`
		TempMethod         string `json:"temp_method"`
		TempMethodNote     string `json:"temp_method_note"`
		LossResponsibility string `json:"loss_responsibility"`
		LossNote           string `json:"loss_note"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.BatchID == 0 {
		fail(w, 400, "请指定 batch_id")
		return
	}
	if !contains(tempMethods, in.TempMethod) {
		fail(w, 400, "临时保温方式须为 ice_pack/quilt/ventilated/shadow/fan/other")
		return
	}
	resp := in.LossResponsibility
	if resp == "" {
		resp = "shared" // 默认共同承担，待实际出险再由双方确认
	}
	if !contains(lossRespOptions, resp) {
		fail(w, 400, "损耗责任须为 farmer/coldstore/wholesaler/shared")
		return
	}
	var eta *time.Time
	if in.PickupETA != "" {
		t, err := parseTime(in.PickupETA)
		if err != nil {
			fail(w, 400, "预计提货时间格式错误，示例 2026-09-27T06:00")
			return
		}
		eta = &t
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var (
		farmerID, bundles         int
		brokerID                  *int
		category, status, batchNo string
		slotID                    *int
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT farmer_id, broker_id, category, bundles, status, COALESCE(batch_no,''), storage_slot_id
		 FROM flower_batches WHERE id=$1 FOR UPDATE`, in.BatchID).
		Scan(&farmerID, &brokerID, &category, &bundles, &status, &batchNo, &slotID)
	if err == sql.ErrNoRows {
		fail(w, 404, "批次不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if slotID != nil {
		fail(w, 409, "该批次已在冷库库位中，无需排队")
		return
	}
	if !contains([]string{"qc_passed", "qc_flagged", "sold"}, status) {
		fail(w, 409, "仅质检通过（或已成交待入库）的批次可排队，当前状态: "+status)
		return
	}
	var active int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT count(*) FROM cold_storage_queue WHERE batch_id=$1 AND status IN ('waiting','admitted')`,
		in.BatchID).Scan(&active); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if active > 0 {
		fail(w, 409, "该批次已在等待入库队列中")
		return
	}

	var sens int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT sensitivity FROM category_sensitivity WHERE category=$1`, category).Scan(&sens); err != nil {
		sens = defaultSensitivity[category]
	}
	var wholesalerID *int
	var isSold bool
	if err := tx.QueryRowContext(r.Context(),
		`SELECT wholesaler_id FROM inventories WHERE batch_id=$1 ORDER BY id LIMIT 1`, in.BatchID).Scan(&wholesalerID); err == nil {
		isSold = true
	}
	score := coldPriorityScore(sens, isSold, eta)

	var qid int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO cold_storage_queue
		 (batch_id, farmer_id, broker_id, wholesaler_id, sensitivity, is_sold, pickup_eta, temp_method,
		  temp_method_note, loss_responsibility, loss_note, priority_score, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		in.BatchID, farmerID, brokerID, wholesalerID, sens, isSold, eta, in.TempMethod,
		in.TempMethodNote, resp, in.LossNote, score, u.ID).Scan(&qid)
	if err != nil {
		fail(w, 500, "排队失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{
		"id": qid, "batch_id": in.BatchID, "batch_no": batchNo, "status": "waiting",
		"sensitivity": sens, "is_sold": isSold, "pickup_eta": eta,
		"temp_method": in.TempMethod, "temp_method_cn": tempMethodCN[in.TempMethod],
		"loss_responsibility": resp, "priority_score": score,
	})
}

const queueSelect = `
 SELECT q.id, q.batch_id, q.farmer_id, COALESCE(q.broker_id,0), COALESCE(q.wholesaler_id,0),
        q.sensitivity, q.is_sold, q.pickup_eta, q.temp_method, q.temp_method_note,
        q.loss_responsibility, q.loss_note, q.priority_score, q.status, q.queued_at, q.admitted_at,
        COALESCE(q.slot_id,0),
        b.batch_no, b.category, b.origin, b.bundles, b.grade, b.openness, b.status,
        f.name, COALESCE(bk.name,''), COALESCE(w.name,''), COALESCE(s.code,''),
        EXISTS(SELECT 1 FROM inventories i WHERE i.batch_id=b.id),
        COALESCE((SELECT wh.name FROM inventories inv JOIN users wh ON wh.id=inv.wholesaler_id
                  WHERE inv.batch_id=b.id ORDER BY inv.id LIMIT 1),'')
 FROM cold_storage_queue q
 JOIN flower_batches b ON b.id=q.batch_id
 JOIN users f ON f.id=q.farmer_id
 LEFT JOIN users bk ON bk.id=q.broker_id
 LEFT JOIN users w ON w.id=q.wholesaler_id
 LEFT JOIN storage_slots s ON s.id=q.slot_id`

const queueOrder = `
 ORDER BY CASE q.status WHEN 'waiting' THEN 0 WHEN 'admitted' THEN 1 ELSE 2 END,
          q.sensitivity DESC,
          EXISTS(SELECT 1 FROM inventories i WHERE i.batch_id=b.id) DESC,
          q.pickup_eta ASC NULLS LAST,
          q.queued_at ASC`

func scanQueueRow(rows *sql.Rows) (map[string]any, error) {
	var (
		qid, batchID, farmerID, brokerID, wholesalerID, sens, bundles, slotID          int
		isSold, soldNow                                                                bool
		pickupETA, admittedAt                                                          *time.Time
		tempMethod, methodNote, lossResp, lossNote, qstatus                            string
		score                                                                          float64
		queuedAt                                                                       time.Time
		batchNo, category, origin, grade, batchStatus, farmer, broker, whole, slotCode string
		openness                                                                       float64
		liveWhole                                                                      string
	)
	if err := rows.Scan(&qid, &batchID, &farmerID, &brokerID, &wholesalerID, &sens, &isSold,
		&pickupETA, &tempMethod, &methodNote, &lossResp, &lossNote, &score, &qstatus, &queuedAt,
		&admittedAt, &slotID, &batchNo, &category, &origin, &bundles, &grade, &openness, &batchStatus,
		&farmer, &broker, &whole, &slotCode, &soldNow, &liveWhole); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": qid, "batch_id": batchID, "farmer_id": farmerID, "broker_id": brokerID,
		"wholesaler_id": wholesalerID, "sensitivity": sens, "is_sold": isSold, "sold_now": soldNow,
		"pickup_eta": pickupETA, "temp_method": tempMethod, "temp_method_cn": tempMethodCN[tempMethod],
		"temp_method_note": methodNote, "loss_responsibility": lossResp,
		"loss_responsibility_cn": lossRespCN[lossResp], "loss_note": lossNote,
		"priority_score": score, "status": qstatus, "queued_at": queuedAt, "admitted_at": admittedAt,
		"slot_id": slotID, "slot_code": slotCode,
		"batch_no": batchNo, "category": category, "category_cn": catCN(category), "origin": origin,
		"bundles": bundles, "grade": grade, "openness": openness, "batch_status": batchStatus,
		"farmer_name": farmer, "broker_name": broker, "wholesaler_name": whole,
		"live_wholesaler": liveWhole,
	}, nil
}

// handleColdQueue 调度队列（冷库/管理看全部；花农/经纪人/批发商只看与自己相关的）
func (a *app) handleColdQueue(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := queueSelect
	args := []any{}
	switch u.Role {
	case "farmer":
		q += " WHERE q.farmer_id=$1"
		args = append(args, u.ID)
	case "broker":
		q += " WHERE q.broker_id=$1"
		args = append(args, u.ID)
	case "wholesaler":
		q += " WHERE q.wholesaler_id=$1 OR EXISTS(SELECT 1 FROM inventories i WHERE i.batch_id=b.id AND i.wholesaler_id=$1)"
		args = append(args, u.ID)
	}
	if st := r.URL.Query().Get("status"); st != "" {
		if len(args) == 0 {
			q += " WHERE q.status=$1"
		} else {
			q += fmt.Sprintf(" AND q.status=$%d", len(args)+1)
		}
		args = append(args, st)
	}
	rows, err := a.db.QueryContext(r.Context(), q+queueOrder+" LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		if m, err := scanQueueRow(rows); err == nil {
			out = append(out, m)
		}
	}
	writeJSON(w, 200, out)
}

// handleAdmit 冷库负责人按调度顺序放行入库（仍校验库位余量）
func (a *app) handleAdmit(w http.ResponseWriter, r *http.Request) {
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

	var batchID int
	var qstatus string
	if err := tx.QueryRowContext(r.Context(),
		`SELECT batch_id, status FROM cold_storage_queue WHERE id=$1 FOR UPDATE`, id).
		Scan(&batchID, &qstatus); err == sql.ErrNoRows {
		fail(w, 404, "排队记录不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if qstatus != "waiting" {
		fail(w, 409, "该排队记录当前状态不可放行: "+qstatus)
		return
	}
	var bundles int
	var bstatus string
	var existedSlot *int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT bundles, status, storage_slot_id FROM flower_batches WHERE id=$1 FOR UPDATE`, batchID).
		Scan(&bundles, &bstatus, &existedSlot); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if existedSlot != nil {
		fail(w, 409, "该批次已在库位中")
		return
	}
	var code string
	var capacity, occupied int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT code, capacity_bundles, occupied_bundles FROM storage_slots WHERE id=$1 FOR UPDATE`, in.SlotID).
		Scan(&code, &capacity, &occupied); err == sql.ErrNoRows {
		fail(w, 404, "库位不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if occupied+bundles > capacity {
		fail(w, 409, fmt.Sprintf("冷库库位仍不足：库位 %s 剩余 %d 扎，本批次需要 %d 扎，请继续等待或更换库位",
			code, capacity-occupied, bundles))
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE storage_slots SET occupied_bundles=occupied_bundles+$1 WHERE id=$2`, bundles, in.SlotID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE flower_batches SET storage_slot_id=$1, stored_at=COALESCE(stored_at,now()),
		 status=CASE WHEN status='sold' THEN 'sold' ELSE 'stored' END WHERE id=$2`, in.SlotID, batchID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE cold_storage_queue SET status='admitted', admitted_at=now(), slot_id=$1 WHERE id=$2`, in.SlotID, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "batch_id": batchID, "status": "admitted", "slot_code": code})
}

// handleReleaseQueue 批次移出等待队列（货主拉走/改投其他冷库，无需占用库位）
func (a *app) handleReleaseQueue(w http.ResponseWriter, r *http.Request) {
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
	if err := tx.QueryRowContext(r.Context(),
		`SELECT status FROM cold_storage_queue WHERE id=$1 FOR UPDATE`, id).Scan(&status); err == sql.ErrNoRows {
		fail(w, 404, "排队记录不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "waiting" {
		fail(w, 409, "仅等待中的记录可移出，当前: "+status)
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE cold_storage_queue SET status='released', released_by=$1, released_at=now() WHERE id=$2`, u.ID, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": "released"})
}

// ---------- 临时保温失败事故 ----------

// handleCreateIncident 上报临时保温失败、花材开放过快
func (a *app) handleCreateIncident(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		BatchID        int     `json:"batch_id"`
		QueueID        *int    `json:"queue_id"`
		OpennessBefore float64 `json:"openness_before"`
		OpennessAfter  float64 `json:"openness_after"`
		TempMethod     string  `json:"temp_method"`
		Description    string  `json:"description"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.BatchID == 0 || in.OpennessAfter <= 0 {
		fail(w, 400, "请给出 batch_id 与开放后开放度 openness_after")
		return
	}
	if in.OpennessBefore < 0 || in.OpennessBefore > 5 || in.OpennessAfter < 1 || in.OpennessAfter > 5 {
		fail(w, 400, "开放度须在 1.0-5.0 之间")
		return
	}
	if in.OpennessBefore > 0 && in.OpennessAfter <= in.OpennessBefore {
		fail(w, 409, "保温失败表现为开放过快，开放后开放度应高于之前")
		return
	}
	method := in.TempMethod
	if method == "" && in.QueueID != nil {
		// 默认带出排队时登记的临时保温方式
		_ = a.db.QueryRow(`SELECT temp_method FROM cold_storage_queue WHERE id=$1`, *in.QueueID).Scan(&method)
	}
	if method != "" && !contains(tempMethods, method) {
		fail(w, 400, "临时保温方式须为 ice_pack/quilt/ventilated/shadow/fan/other")
		return
	}
	var batchNo, category string
	var farmerID int
	var brokerID *int
	err := a.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(batch_no,''), category, farmer_id, broker_id FROM flower_batches WHERE id=$1`, in.BatchID).
		Scan(&batchNo, &category, &farmerID, &brokerID)
	if err == sql.ErrNoRows {
		fail(w, 404, "批次不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if in.QueueID != nil {
		var qb int
		var qstatus string
		if err := a.db.QueryRowContext(r.Context(),
			`SELECT batch_id, status FROM cold_storage_queue WHERE id=$1`, *in.QueueID).
			Scan(&qb, &qstatus); err != nil {
			fail(w, 404, "关联排队记录不存在")
			return
		}
		if qb != in.BatchID {
			fail(w, 400, "排队记录与批次不一致")
			return
		}
		if qstatus == "waiting" {
			// 保温失败，退出优先入库等待
			a.db.Exec(`UPDATE cold_storage_queue SET status='failed' WHERE id=$1 AND status='waiting'`, *in.QueueID)
		}
	}

	var iid int
	err = a.db.QueryRowContext(r.Context(),
		`INSERT INTO cold_chain_incidents (queue_id, batch_id, reported_by, openness_before, openness_after,
		 description, temp_method)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		in.QueueID, in.BatchID, u.ID, nilIfZero(in.OpennessBefore), in.OpennessAfter, in.Description, method).Scan(&iid)
	if err != nil {
		fail(w, 500, "上报失败: "+err.Error())
		return
	}

	title := fmt.Sprintf("保温失败待确认责任：批次 %s（%s）开放过快", batchNo, catCN(category))
	body := fmt.Sprintf("开放度 %.1f → %.1f，请冷库负责人与批发商分别确认责任", in.OpennessBefore, in.OpennessAfter)
	a.notifyRole(r.Context(), "coldstore", "incident", title, body, "incident", iid, false)
	var wholesalerID int
	if a.db.QueryRowContext(r.Context(),
		`SELECT wholesaler_id FROM inventories WHERE batch_id=$1 ORDER BY id LIMIT 1`, in.BatchID).Scan(&wholesalerID) == nil {
		a.notify(r.Context(), wholesalerID, "incident", title, body, "incident", iid, false)
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{
		"id": iid, "batch_id": in.BatchID, "status": "pending",
		"openness_before": in.OpennessBefore, "openness_after": in.OpennessAfter,
		"message": "已通知冷库负责人与批发商确认责任",
	})
}

func nilIfZero(f float64) *float64 {
	if f == 0 {
		return nil
	}
	return &f
}

func (a *app) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT i.id, i.queue_id, i.batch_id, i.reported_by, i.opened_at, i.openness_before, i.openness_after,
	        i.description, i.temp_method, i.coldstore_responsible, i.wholesaler_responsible,
	        i.coldstore_confirmed_at, i.wholesaler_confirmed_at, i.status, i.reviewed_at, i.review_note,
	        i.adjustment_id, i.closed_at,
	        b.batch_no, b.category, b.origin, COALESCE(b.broker_id,0), b.farmer_id,
	        ru.name, COALESCE(cu.name,''), COALESCE(wu.name,''),
	        COALESCE((SELECT inv.wholesaler_id FROM inventories inv WHERE inv.batch_id=b.id ORDER BY inv.id LIMIT 1),0)
	     FROM cold_chain_incidents i
	     JOIN flower_batches b ON b.id=i.batch_id
	     JOIN users ru ON ru.id=i.reported_by
	     LEFT JOIN users cu ON cu.id=i.coldstore_confirmed_by
	     LEFT JOIN users wu ON wu.id=i.wholesaler_confirmed_by`
	args := []any{}
	switch u.Role {
	case "farmer":
		q += " WHERE b.farmer_id=$1"
		args = append(args, u.ID)
	case "broker":
		q += " WHERE b.broker_id=$1"
		args = append(args, u.ID)
	case "wholesaler":
		q += " WHERE EXISTS(SELECT 1 FROM inventories inv WHERE inv.batch_id=b.id AND inv.wholesaler_id=$1)"
		args = append(args, u.ID)
	}
	if st := r.URL.Query().Get("status"); st != "" {
		if len(args) == 0 {
			q += " WHERE i.status=$1"
		} else {
			q += fmt.Sprintf(" AND i.status=$%d", len(args)+1)
		}
		args = append(args, st)
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY i.id DESC LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			iid, batchID, reportedBy, brokerID, farmerID, liveWholesalerID int
			queueID, adjustmentID                                          *int
			openBefore                                                     *float64
			openAfter                                                      float64
			coldAt, wholeAt, reviewedAt, closedAt                          *time.Time
			openedAt                                                       time.Time
			coldResp, wholeResp                                            bool
			desc, method, status, reviewNote, batchNo, category, origin    string
			reporter, coldBy, wholeBy                                      string
		)
		if err := rows.Scan(&iid, &queueID, &batchID, &reportedBy, &openedAt, &openBefore, &openAfter,
			&desc, &method, &coldResp, &wholeResp, &coldAt, &wholeAt, &status, &reviewedAt, &reviewNote,
			&adjustmentID, &closedAt, &batchNo, &category, &origin, &brokerID, &farmerID,
			&reporter, &coldBy, &wholeBy, &liveWholesalerID); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": iid, "queue_id": queueID, "batch_id": batchID, "reporter": reporter,
			"opened_at": openedAt, "openness_before": openBefore, "openness_after": openAfter,
			"description": desc, "temp_method": method, "temp_method_cn": tempMethodCN[method],
			"coldstore_responsible": coldResp, "wholesaler_responsible": wholeResp,
			"coldstore_confirmed_at": coldAt, "wholesaler_confirmed_at": wholeAt,
			"coldstore_confirmed_by": coldBy, "wholesaler_confirmed_by": wholeBy,
			"status": status, "reviewed_at": reviewedAt, "review_note": reviewNote,
			"adjustment_id": adjustmentID, "closed_at": closedAt,
			"batch_no": batchNo, "category": category, "category_cn": catCN(category), "origin": origin,
			"broker_id": brokerID, "farmer_id": farmerID, "live_wholesaler_id": liveWholesalerID,
		})
	}
	writeJSON(w, 200, out)
}

// handleConfirmIncident 冷库负责人 / 批发商分别确认责任；双方确认齐后转 confirmed
func (a *app) handleConfirmIncident(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Side        string `json:"side"` // coldstore/wholesaler（manager 必填）
		Responsible bool   `json:"responsible"`
		Note        string `json:"note"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	side := in.Side
	switch u.Role {
	case "coldstore":
		side = "coldstore"
	case "wholesaler":
		side = "wholesaler"
	case "manager":
		if side != "coldstore" && side != "wholesaler" {
			fail(w, 400, "市场代确认时须指定 side=coldstore|wholesaler")
			return
		}
	default:
		fail(w, 403, "仅冷库负责人、批发商（或市场管理代行）可确认")
		return
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var batchID int
	var status string
	var liveWholesaler int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT i.batch_id, i.status,
		        COALESCE((SELECT inv.wholesaler_id FROM inventories inv WHERE inv.batch_id=i.batch_id ORDER BY inv.id LIMIT 1),0)
		 FROM cold_chain_incidents i WHERE i.id=$1 FOR UPDATE`, id).Scan(&batchID, &status, &liveWholesaler); err == sql.ErrNoRows {
		fail(w, 404, "事故不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status == "reviewed" {
		fail(w, 409, "事故已复盘结案")
		return
	}
	if side == "wholesaler" && u.Role == "wholesaler" && liveWholesaler != 0 && liveWholesaler != u.ID {
		fail(w, 403, "仅该批次拍得批发商可确认批发商侧责任")
		return
	}
	var batchNo string
	tx.QueryRow(`SELECT COALESCE(batch_no,'') FROM flower_batches WHERE id=$1`, batchID).Scan(&batchNo)

	if side == "coldstore" {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE cold_chain_incidents
			 SET coldstore_responsible=$1, coldstore_confirmed_by=$2, coldstore_confirmed_at=now()
			 WHERE id=$3`, in.Responsible, u.ID, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
	} else {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE cold_chain_incidents
			 SET wholesaler_responsible=$1, wholesaler_confirmed_by=$2, wholesaler_confirmed_at=now()
			 WHERE id=$3`, in.Responsible, u.ID, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}

	var coldAt, wholeAt *time.Time
	if err := tx.QueryRowContext(r.Context(),
		`SELECT coldstore_confirmed_at, wholesaler_confirmed_at FROM cold_chain_incidents WHERE id=$1`, id).
		Scan(&coldAt, &wholeAt); err != nil {
		fail(w, 500, err.Error())
		return
	}
	bothConfirmed := coldAt != nil && wholeAt != nil
	if bothConfirmed {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE cold_chain_incidents SET status='confirmed' WHERE id=$1 AND status='pending'`, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}

	// 通知对方与冷库复盘人
	if side == "coldstore" {
		if liveWholesaler != 0 {
			a.notify(r.Context(), liveWholesaler, "incident",
				fmt.Sprintf("冷库已确认事故 #%d 责任，待批发商确认", id),
				fmt.Sprintf("批次 %s", batchNo), "incident", id, false)
		}
	} else {
		a.notifyRole(r.Context(), "coldstore", "incident",
			fmt.Sprintf("批发商已确认事故 #%d 责任，待冷库确认", id),
			fmt.Sprintf("批次 %s", batchNo), "incident", id, false)
	}
	if bothConfirmed {
		a.notifyRole(r.Context(), "coldstore", "incident",
			fmt.Sprintf("事故 #%d 双方责任已确认，请冷库端同步复盘", id),
			fmt.Sprintf("批次 %s，复盘后市场可调整该时段入场顺序", batchNo), "incident", id, false)
		a.notifyRole(r.Context(), "manager", "incident",
			fmt.Sprintf("事故 #%d 双方责任已确认，冷库复盘中", id),
			fmt.Sprintf("批次 %s", batchNo), "incident", id, false)
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{
		"id": id, "side": side, "responsible": in.Responsible,
		"status":         map[bool]string{true: "confirmed", false: "pending"}[bothConfirmed],
		"both_confirmed": bothConfirmed,
	})
}

// handleReviewIncident 冷库端同步复盘（双方确认责任之后）
func (a *app) handleReviewIncident(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Note string `json:"note"`
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
	var batchID int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT status, batch_id FROM cold_chain_incidents WHERE id=$1 FOR UPDATE`, id).Scan(&status, &batchID); err == sql.ErrNoRows {
		fail(w, 404, "事故不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status == "pending" {
		fail(w, 409, "冷库负责人与批发商尚未完成双方责任确认，不能复盘")
		return
	}
	if status == "reviewed" {
		fail(w, 409, "该事故已复盘结案")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE cold_chain_incidents SET status='reviewed', reviewed_by=$1, reviewed_at=now(),
		 review_note=$2, closed_at=now() WHERE id=$3`, u.ID, in.Note, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	var batchNo string
	a.db.QueryRow(`SELECT COALESCE(batch_no,'') FROM flower_batches WHERE id=$1`, batchID).Scan(&batchNo)
	a.notifyRole(r.Context(), "manager", "incident",
		fmt.Sprintf("事故 #%d 冷库复盘完成，可发起入场顺序调整", id),
		fmt.Sprintf("批次 %s：%s", batchNo, in.Note), "incident", id, false)
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": "reviewed", "review_note": in.Note})
}

// ---------- 入场顺序调整：市场发起 → 通知经纪人 → 经纪人确认 ----------

// handleCreateAdjustment 市场管理方依据复盘结论调整某时段凌晨入场顺序
func (a *app) handleCreateAdjustment(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		TimeWindow    string `json:"time_window"`
		Adjustment    string `json:"adjustment"`
		EffectiveDate string `json:"effective_date"` // YYYY-MM-DD，缺省今天
		BrokerID      *int   `json:"broker_id"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.TimeWindow == "" || in.Adjustment == "" {
		fail(w, 400, "请给出调整时段 time_window 与调整内容 adjustment")
		return
	}
	effDate := time.Now()
	if in.EffectiveDate != "" {
		if t, err := time.ParseInLocation("2006-01-02", in.EffectiveDate, time.Local); err == nil {
			effDate = t
		}
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var status string
	var batchID int
	var existingAdj *int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT status, batch_id, adjustment_id FROM cold_chain_incidents WHERE id=$1 FOR UPDATE`, id).
		Scan(&status, &batchID, &existingAdj); err == sql.ErrNoRows {
		fail(w, 404, "事故不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "reviewed" {
		fail(w, 409, "须冷库端复盘完成后才能调整入场顺序，当前事故状态: "+status)
		return
	}
	if existingAdj != nil {
		fail(w, 409, "该事故已发起过入场顺序调整")
		return
	}
	var batchBroker *int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT broker_id FROM flower_batches WHERE id=$1`, batchID).Scan(&batchBroker); err != nil {
		fail(w, 500, err.Error())
		return
	}
	brokerID := in.BrokerID
	if brokerID == nil {
		brokerID = batchBroker // 缺省通知该批次代录经纪人
	}
	var wholesalerID *int
	tx.QueryRowContext(r.Context(),
		`SELECT wholesaler_id FROM inventories WHERE batch_id=$1 ORDER BY id LIMIT 1`, batchID).Scan(&wholesalerID)

	var batchNo, category string
	tx.QueryRow(`SELECT COALESCE(batch_no,''), category FROM flower_batches WHERE id=$1`, batchID).Scan(&batchNo, &category)

	var adjID int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO entry_order_adjustments
		 (incident_id, time_window, wholesaler_id, broker_id, adjustment, effective_date, status, created_by, notified_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'notified',$7,now()) RETURNING id`,
		id, in.TimeWindow, wholesalerID, brokerID, in.Adjustment, effDate, u.ID).Scan(&adjID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE cold_chain_incidents SET adjustment_id=$1 WHERE id=$2`, adjID, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}

	title := fmt.Sprintf("凌晨入场顺序调整通知（%s 时段，%s 生效）", in.TimeWindow, effDate.Format("2006-01-02"))
	body := fmt.Sprintf("依据批次 %s（%s）保温失败复盘，%s。请确认知悉。", batchNo, catCN(category), in.Adjustment)
	if brokerID != nil && *brokerID != 0 {
		a.notify(r.Context(), *brokerID, "entry_adjustment", title, body, "adjustment", adjID, true)
	} else {
		// 批次没有固定经纪人：通知全部经纪人
		a.notifyRole(r.Context(), "broker", "entry_adjustment", title, body, "adjustment", adjID, true)
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{
		"id": adjID, "incident_id": id, "status": "notified",
		"time_window": in.TimeWindow, "effective_date": effDate.Format("2006-01-02"),
		"broker_id": brokerID, "message": "入场顺序调整已通知经纪人，等待确认",
	})
}

// handleListAdjustments 入场顺序调整列表
func (a *app) handleListAdjustments(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT e.id, e.incident_id, e.time_window, COALESCE(e.wholesaler_id,0), COALESCE(e.broker_id,0),
	        e.adjustment, e.effective_date, e.status, e.created_at, e.notified_at,
	        e.broker_confirmed_at, e.broker_note,
	        b.batch_no, b.category, COALESCE(wu.name,''), COALESCE(bu.name,''), COALESCE(mu.name,'')
	     FROM entry_order_adjustments e
	     JOIN cold_chain_incidents i ON i.id=e.incident_id
	     JOIN flower_batches b ON b.id=i.batch_id
	     LEFT JOIN users wu ON wu.id=e.wholesaler_id
	     LEFT JOIN users bu ON bu.id=e.broker_id
	     LEFT JOIN users mu ON mu.id=e.created_by`
	args := []any{}
	if u.Role == "broker" {
		q += " WHERE e.broker_id=$1 OR e.broker_id IS NULL"
		args = append(args, u.ID)
	} else if u.Role == "wholesaler" {
		q += " WHERE e.wholesaler_id=$1"
		args = append(args, u.ID)
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY e.id DESC LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			adjID, incidentID, wholesalerID, brokerID                   int
			timeWindow, adjustment, status                              string
			effDate                                                     time.Time
			createdAt                                                   time.Time
			notifiedAt, confirmedAt                                     *time.Time
			note, batchNo, category, wholeName, brokerName, managerName string
		)
		if err := rows.Scan(&adjID, &incidentID, &timeWindow, &wholesalerID, &brokerID, &adjustment,
			&effDate, &status, &createdAt, &notifiedAt, &confirmedAt, &note,
			&batchNo, &category, &wholeName, &brokerName, &managerName); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": adjID, "incident_id": incidentID, "time_window": timeWindow,
			"wholesaler_id": wholesalerID, "broker_id": brokerID, "adjustment": adjustment,
			"effective_date": effDate.Format("2006-01-02"), "status": status,
			"created_at": createdAt, "notified_at": notifiedAt, "broker_confirmed_at": confirmedAt,
			"broker_note": note, "batch_no": batchNo, "category": category, "category_cn": catCN(category),
			"wholesaler_name": wholeName, "broker_name": brokerName, "created_by_name": managerName,
		})
	}
	writeJSON(w, 200, out)
}

// handleBrokerConfirmAdjustment 经纪人确认（或反馈异议）入场顺序调整
func (a *app) handleBrokerConfirmAdjustment(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Accept bool   `json:"accept"`
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
	var brokerID *int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT status, broker_id FROM entry_order_adjustments WHERE id=$1 FOR UPDATE`, id).Scan(&status, &brokerID); err == sql.ErrNoRows {
		fail(w, 404, "入场调整不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "notified" && status != "proposed" {
		fail(w, 409, "该调整已处理，当前: "+status)
		return
	}
	if brokerID != nil && *brokerID != 0 && *brokerID != u.ID {
		fail(w, 403, "仅被通知的经纪人可确认")
		return
	}
	newStatus := "confirmed"
	if !in.Accept {
		newStatus = "rejected"
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE entry_order_adjustments SET status=$1, broker_confirmed_by=$2, broker_confirmed_at=now(),
		 broker_note=$3 WHERE id=$4`, newStatus, u.ID, in.Note, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 确认后把对应通知标记为已确认
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE notifications SET acked=false, is_read=true WHERE ref_type='adjustment' AND ref_id=$1 AND user_id=$2`,
		id, u.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.notifyRole(r.Context(), "manager", "entry_adjustment",
		fmt.Sprintf("经纪人 %s 已%s入场调整 #%d", u.Name, map[bool]string{true: "确认", false: "反馈异议"}[in.Accept], id),
		in.Note, "adjustment", id, false)
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": newStatus, "broker": u.Name})
}

// ---------- 通知 ----------

func (a *app) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT id, type, title, body, ref_type, ref_id, is_read, acked, created_at
	      FROM notifications WHERE user_id=$1`
	args := []any{u.ID}
	if r.URL.Query().Get("unread") == "1" {
		q += " AND is_read=false"
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY id DESC LIMIT 100", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, refID int
		var typ, title, body, refType string
		var isRead, acked bool
		var at time.Time
		if rows.Scan(&id, &typ, &title, &body, &refType, &refID, &isRead, &acked, &at) == nil {
			out = append(out, map[string]any{"id": id, "type": typ, "title": title, "body": body,
				"ref_type": refType, "ref_id": refID, "is_read": isRead, "need_ack": acked, "created_at": at})
		}
	}
	var unread int64
	a.db.QueryRowContext(r.Context(), `SELECT count(*) FROM notifications WHERE user_id=$1 AND is_read=false`, u.ID).Scan(&unread)
	writeJSON(w, 200, map[string]any{"unread_count": unread, "items": out})
}

func (a *app) handleReadNotification(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	if _, err := a.db.ExecContext(r.Context(),
		`UPDATE notifications SET is_read=true WHERE id=$1 AND user_id=$2`, id, u.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "is_read": true})
}

func (a *app) handleReadAllNotifications(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	// 仅清普通通知，需要经纪人确认的入场调整通知保留
	if _, err := a.db.ExecContext(r.Context(),
		`UPDATE notifications SET is_read=true WHERE user_id=$1 AND acked=false`, u.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 冷库调度看板 ----------

func (a *app) handleColdBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{}

	var waiting, failed, pendingInc, confirmedInc, reviewedInc, adjNotified, adjConfirmed int64
	a.db.QueryRowContext(ctx, `SELECT count(*) FROM cold_storage_queue WHERE status='waiting'`).Scan(&waiting)
	a.db.QueryRowContext(ctx, `SELECT count(*) FROM cold_storage_queue WHERE status='failed'`).Scan(&failed)
	a.db.QueryRowContext(ctx, `SELECT count(*) FROM cold_chain_incidents WHERE status='pending'`).Scan(&pendingInc)
	a.db.QueryRowContext(ctx, `SELECT count(*) FROM cold_chain_incidents WHERE status='confirmed'`).Scan(&confirmedInc)
	a.db.QueryRowContext(ctx, `SELECT count(*) FROM cold_chain_incidents WHERE status='reviewed'`).Scan(&reviewedInc)
	a.db.QueryRowContext(ctx, `SELECT count(*) FROM entry_order_adjustments WHERE status='notified'`).Scan(&adjNotified)
	a.db.QueryRowContext(ctx, `SELECT count(*) FROM entry_order_adjustments WHERE status='confirmed'`).Scan(&adjConfirmed)
	out["counts"] = map[string]int64{
		"waiting_queue":         waiting,
		"failed_temp_storage":   failed,
		"incidents_pending":     pendingInc,
		"incidents_confirmed":   confirmedInc,
		"incidents_reviewed":    reviewedInc,
		"adjustments_notified":  adjNotified,
		"adjustments_confirmed": adjConfirmed,
	}

	// 库位占用
	slots := []map[string]any{}
	rows, err := a.db.QueryContext(ctx,
		`SELECT id, code, zone, capacity_bundles, occupied_bundles FROM storage_slots ORDER BY code`)
	if err == nil {
		for rows.Next() {
			var id, cap, occ int
			var code, zone string
			if rows.Scan(&id, &code, &zone, &cap, &occ) == nil {
				slots = append(slots, map[string]any{"id": id, "code": code, "zone": zone,
					"capacity_bundles": cap, "occupied_bundles": occ, "free_bundles": cap - occ})
			}
		}
		rows.Close()
	}
	out["slots"] = slots
	out["cold_storage_full"] = len(slots) > 0 && waiting > 0

	// 调度顺序（等待中前 20）
	rows, err = a.db.QueryContext(ctx, queueSelect+` WHERE q.status='waiting'`+queueOrder+` LIMIT 20`)
	queue := []map[string]any{}
	if err == nil {
		for rows.Next() {
			if m, e := scanQueueRow(rows); e == nil {
				queue = append(queue, m)
			}
		}
		rows.Close()
	}
	out["waiting_queue"] = queue

	writeJSON(w, 200, out)
}
