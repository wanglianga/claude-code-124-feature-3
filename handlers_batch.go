package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---------- 花材批次 ----------

// handleCreateBatch 花农/经纪人凌晨到货登记
func (a *app) handleCreateBatch(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		FarmerID         int     `json:"farmer_id"` // 经纪人代录时指定花农
		Category         string  `json:"category"`
		Origin           string  `json:"origin"`
		CutAt            string  `json:"cut_at"`
		StemLength       float64 `json:"stem_length_cm"`
		Openness         float64 `json:"openness"`
		Bundles          int     `json:"bundles"`
		StemsPerBundle   int     `json:"stems_per_bundle"`
		ArrivalTemp      float64 `json:"arrival_temp_c"`
		SellableHours    int     `json:"sellable_hours"`
		Sensitivity      string  `json:"sensitivity"`        // 花材敏感度 high/medium/low，空则按品类默认
		ExpectedPickupAt string  `json:"expected_pickup_at"` // 预计提货时间（可选，影响爆满时入库优先级）
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if !contains(categories, in.Category) {
		fail(w, 400, "品类须为 rose/lily/tulip/eustoma/eucalyptus/holiday_mix")
		return
	}
	if strings.TrimSpace(in.Origin) == "" || in.Bundles <= 0 || in.StemLength <= 0 {
		fail(w, 400, "产地、扎数、枝长必填且大于 0")
		return
	}
	if in.Openness < 1 || in.Openness > 5 {
		fail(w, 400, "开放度须在 1.0-5.0 之间")
		return
	}
	if in.SellableHours < 4 || in.SellableHours > 240 {
		fail(w, 400, "可售时长须在 4-240 小时之间")
		return
	}
	if in.StemsPerBundle <= 0 {
		in.StemsPerBundle = 20
	}
	sensitivity := in.Sensitivity
	if sensitivity == "" {
		sensitivity = categorySensitivity[in.Category]
	}
	if !contains(sensitivityLevels, sensitivity) {
		fail(w, 400, "敏感度须为 high/medium/low")
		return
	}
	var expectedPickup *time.Time
	if in.ExpectedPickupAt != "" {
		t, err := parseTime(in.ExpectedPickupAt)
		if err != nil {
			fail(w, 400, "预计提货时间格式错误，示例 2026-09-27T08:30")
			return
		}
		expectedPickup = &t
	}
	cutAt, err := parseTime(in.CutAt)
	if err != nil {
		fail(w, 400, "采切时间格式错误，示例 2026-09-25T03:30")
		return
	}

	farmerID := u.ID
	var brokerID *int
	if u.Role == "broker" {
		if in.FarmerID == 0 {
			fail(w, 400, "经纪人代录需指定 farmer_id")
			return
		}
		var role string
		if err := a.db.QueryRowContext(r.Context(), `SELECT role FROM users WHERE id=$1`, in.FarmerID).Scan(&role); err != nil || role != "farmer" {
			fail(w, 400, "指定的花农不存在")
			return
		}
		farmerID = in.FarmerID
		brokerID = &u.ID
	}

	// 冷链到货温度异常即时打标
	riskFlags := ""
	if in.ArrivalTemp > 8 {
		riskFlags = "temp_abnormal"
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO flower_batches (farmer_id, broker_id, category, origin, cut_at, stem_length_cm, openness,
		  bundles, stems_per_bundle, arrival_temp_c, sellable_hours, risk_flags, sensitivity, expected_pickup_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		farmerID, brokerID, in.Category, in.Origin, cutAt, in.StemLength, in.Openness,
		in.Bundles, in.StemsPerBundle, in.ArrivalTemp, in.SellableHours, riskFlags, sensitivity, expectedPickup).Scan(&id)
	if err != nil {
		fail(w, 500, "登记失败: "+err.Error())
		return
	}
	batchNo := fmt.Sprintf("B%06d", id)
	if _, err := tx.ExecContext(r.Context(), `UPDATE flower_batches SET batch_no=$1 WHERE id=$2`, batchNo, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "batch_no": batchNo, "status": "qc_pending",
		"risk_flags": riskFlags, "sensitivity": sensitivity, "expected_pickup_at": expectedPickup})
}

// handleListBatches 批次列表（花农看自己，经纪人看代录，其余看全部）
func (a *app) handleListBatches(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT b.id, b.batch_no, b.category, b.origin, b.cut_at, b.stem_length_cm, b.openness, b.bundles,
	        b.stems_per_bundle, b.arrival_temp_c, b.sellable_hours, b.grade, b.risk_flags, b.status,
	        b.stored_at, b.created_at, f.name, COALESCE(s.code,''), b.sensitivity, b.expected_pickup_at
	      FROM flower_batches b
	      JOIN users f ON f.id=b.farmer_id
	      LEFT JOIN storage_slots s ON s.id=b.storage_slot_id`
	var args []any
	where := ""
	switch u.Role {
	case "farmer":
		where = " WHERE b.farmer_id=$1"
		args = append(args, u.ID)
	case "broker":
		where = " WHERE b.broker_id=$1"
		args = append(args, u.ID)
	}
	if st := r.URL.Query().Get("status"); st != "" {
		if where == "" {
			where = " WHERE b.status=$1"
		} else {
			where += fmt.Sprintf(" AND b.status=$%d", len(args)+1)
		}
		args = append(args, st)
	}
	rows, err := a.db.QueryContext(r.Context(), q+where+" ORDER BY b.id DESC LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id, bundles, spb, sellH                                  int
			batchNo, cat, origin, grade, flags, status, farmer, slot string
			stem, open, temp                                         float64
			cutAt, createdAt                                         time.Time
			storedAt                                                 *time.Time
			sensitivity                                              string
			expectedPickup                                           *time.Time
		)
		if err := rows.Scan(&id, &batchNo, &cat, &origin, &cutAt, &stem, &open, &bundles, &spb,
			&temp, &sellH, &grade, &flags, &status, &storedAt, &createdAt, &farmer, &slot,
			&sensitivity, &expectedPickup); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "batch_no": batchNo, "category": cat, "origin": origin, "cut_at": cutAt,
			"stem_length_cm": stem, "openness": open, "bundles": bundles, "stems_per_bundle": spb,
			"arrival_temp_c": temp, "sellable_hours": sellH, "grade": grade, "risk_flags": flags,
			"status": status, "stored_at": storedAt, "created_at": createdAt, "farmer_name": farmer, "slot_code": slot,
			"sensitivity": sensitivity, "expected_pickup_at": expectedPickup,
		})
	}
	writeJSON(w, 200, out)
}

// handleGetBatch 批次详情（含质检记录、保鲜剂施用）
func (a *app) handleGetBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var (
		bundle                                             map[string]any
		batchNo, cat, origin, grade, flags, status, farmer string
		stem, open, temp                                   float64
		bundles, spb, sellH                                int
		cutAt, createdAt                                   time.Time
		storedAt, outAt                                    *time.Time
		slotCode, sensitivity                              string
		expectedPickup                                     *time.Time
	)
	err := a.db.QueryRowContext(r.Context(),
		`SELECT b.batch_no, b.category, b.origin, b.cut_at, b.stem_length_cm, b.openness, b.bundles,
		        b.stems_per_bundle, b.arrival_temp_c, b.sellable_hours, b.grade, b.risk_flags, b.status,
		        b.stored_at, b.out_at, b.created_at, f.name, COALESCE(s.code,''), b.sensitivity, b.expected_pickup_at
		 FROM flower_batches b JOIN users f ON f.id=b.farmer_id
		 LEFT JOIN storage_slots s ON s.id=b.storage_slot_id WHERE b.id=$1`, id).
		Scan(&batchNo, &cat, &origin, &cutAt, &stem, &open, &bundles, &spb, &temp, &sellH,
			&grade, &flags, &status, &storedAt, &outAt, &createdAt, &farmer, &slotCode,
			&sensitivity, &expectedPickup)
	if err == sql.ErrNoRows {
		fail(w, 404, "批次不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	bundle = map[string]any{
		"id": id, "batch_no": batchNo, "category": cat, "origin": origin, "cut_at": cutAt,
		"stem_length_cm": stem, "openness": open, "bundles": bundles, "stems_per_bundle": spb,
		"arrival_temp_c": temp, "sellable_hours": sellH, "grade": grade, "risk_flags": flags,
		"status": status, "stored_at": storedAt, "out_at": outAt, "created_at": createdAt,
		"farmer_name": farmer, "slot_code": slotCode,
		"sensitivity": sensitivity, "expected_pickup_at": expectedPickup,
	}

	qc := []map[string]any{}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT q.id, q.grade, q.openness_evenness, q.short_stem_ratio, q.temp_ok, q.notes, q.created_at, u.name
		 FROM qc_records q JOIN users u ON u.id=q.inspector_id WHERE q.batch_id=$1 ORDER BY q.id DESC`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var qid int
			var g, notes, insp string
			var even, short float64
			var tempOK bool
			var at time.Time
			if rows.Scan(&qid, &g, &even, &short, &tempOK, &notes, &at, &insp) == nil {
				qc = append(qc, map[string]any{"id": qid, "grade": g, "openness_evenness": even,
					"short_stem_ratio": short, "temp_ok": tempOK, "notes": notes, "created_at": at, "inspector": insp})
			}
		}
	}
	bundle["qc_records"] = qc

	pres := []map[string]any{}
	rows2, err := a.db.QueryContext(r.Context(),
		`SELECT bp.id, p.code, p.brand, bp.dosage_ml, bp.applied_at, u.name
		 FROM batch_preservatives bp JOIN preservative_batches p ON p.id=bp.preservative_id
		 JOIN users u ON u.id=bp.applied_by WHERE bp.batch_id=$1 ORDER BY bp.id DESC`, id)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var pid int
			var code, brand, by string
			var dose float64
			var at time.Time
			if rows2.Scan(&pid, &code, &brand, &dose, &at, &by) == nil {
				pres = append(pres, map[string]any{"id": pid, "code": code, "brand": brand,
					"dosage_ml": dose, "applied_at": at, "applied_by": by})
			}
		}
	}
	bundle["preservatives"] = pres
	writeJSON(w, 200, bundle)
}

// ---------- 质检 ----------

func (a *app) handleQCPending(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT b.id, b.batch_no, b.category, b.origin, b.openness, b.bundles, b.arrival_temp_c, b.created_at, f.name
		 FROM flower_batches b JOIN users f ON f.id=b.farmer_id
		 WHERE b.status='qc_pending' ORDER BY b.created_at`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, bundles int
		var batchNo, cat, origin, farmer string
		var open, temp float64
		var at time.Time
		if rows.Scan(&id, &batchNo, &cat, &origin, &open, &bundles, &temp, &at, &farmer) == nil {
			out = append(out, map[string]any{"id": id, "batch_no": batchNo, "category": cat, "origin": origin,
				"openness": open, "bundles": bundles, "arrival_temp_c": temp, "created_at": at, "farmer_name": farmer})
		}
	}
	writeJSON(w, 200, out)
}

// handleQC 质检定级：开放度不均/短枝比例过高自动打风险标，D 级不得上拍
func (a *app) handleQC(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Grade            string  `json:"grade"`
		OpennessEvenness float64 `json:"openness_evenness"` // 0-1，越低越不均
		ShortStemRatio   float64 `json:"short_stem_ratio"`  // 0-1
		TempOK           bool    `json:"temp_ok"`
		Notes            string  `json:"notes"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if !contains([]string{"A", "B", "C", "D"}, in.Grade) {
		fail(w, 400, "等级须为 A/B/C/D")
		return
	}
	if in.OpennessEvenness < 0 || in.OpennessEvenness > 1 || in.ShortStemRatio < 0 || in.ShortStemRatio > 1 {
		fail(w, 400, "均匀度/短枝比例须在 0-1 之间")
		return
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var status, oldFlags string
	var arrivalTemp float64
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, risk_flags, arrival_temp_c FROM flower_batches WHERE id=$1 FOR UPDATE`, id).
		Scan(&status, &oldFlags, &arrivalTemp)
	if err == sql.ErrNoRows {
		fail(w, 404, "批次不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "qc_pending" {
		fail(w, 409, "该批次已质检，当前状态: "+status)
		return
	}

	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO qc_records (batch_id, inspector_id, grade, openness_evenness, short_stem_ratio, temp_ok, notes)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, u.ID, in.Grade, in.OpennessEvenness, in.ShortStemRatio, in.TempOK, in.Notes); err != nil {
		fail(w, 500, err.Error())
		return
	}

	flags := map[string]bool{}
	for _, f := range strings.Split(oldFlags, ",") {
		if f != "" {
			flags[f] = true
		}
	}
	if in.OpennessEvenness < 0.7 {
		flags["uneven_openness"] = true // 开放度不均
	}
	if in.ShortStemRatio > 0.2 {
		flags["high_short_stem"] = true // 短枝比例过高
	}
	if !in.TempOK || arrivalTemp > 8 {
		flags["temp_abnormal"] = true // 冷链失温
	}
	flagList := []string{}
	for f := range flags {
		flagList = append(flagList, f)
	}
	newStatus := "qc_passed"
	if in.Grade == "D" {
		newStatus = "qc_flagged" // D 级批次打回，不得上拍
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE flower_batches SET grade=$1, risk_flags=$2, status=$3 WHERE id=$4`,
		in.Grade, strings.Join(flagList, ","), newStatus, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "grade": in.Grade, "status": newStatus, "risk_flags": strings.Join(flagList, ",")})
}

// ---------- 冷库 ----------

func (a *app) handleListSlots(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT s.id, s.code, s.zone, s.capacity_bundles, s.occupied_bundles,
		        COALESCE((SELECT count(*) FROM flower_batches b WHERE b.storage_slot_id=s.id AND b.out_at IS NULL
		          AND b.status IN ('stored','in_auction','qc_passed')),0)
		 FROM storage_slots s ORDER BY s.code`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, cap, occ, batches int
		var code, zone string
		if rows.Scan(&id, &code, &zone, &cap, &occ, &batches) == nil {
			out = append(out, map[string]any{"id": id, "code": code, "zone": zone, "capacity_bundles": cap,
				"occupied_bundles": occ, "free_bundles": cap - occ, "batch_count": batches})
		}
	}
	writeJSON(w, 200, out)
}

// handleStoreBatch 入库：库位不足返回 409，并附带按「敏感度→已成交→预计提货时间」排序的调度队列
func (a *app) handleStoreBatch(w http.ResponseWriter, r *http.Request) {
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
	code, err := storeBatchTx(r.Context(), tx, id, in.SlotID)
	if err != nil {
		if se, ok2 := err.(*slotFullError); ok2 {
			// 冷库爆满：返回 409 + 优先入库调度队列，引导先保高优先级批次、其余登记临时保温
			queue, _ := dispatchQueue(r.Context(), tx)
			writeJSON(w, 409, map[string]any{"error": se.msg,
				"free_bundles": freeBundles(r.Context(), tx), "queue": queue})
			return
		}
		status := 409
		if err.Error() == "批次不存在" || err.Error() == "库位不存在" {
			status = 404
		}
		fail(w, status, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": "stored", "slot_code": code})
}

// ---------- 保鲜剂批次 ----------

func (a *app) handleListPreservatives(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, code, brand, ingredient, expires_at, created_at FROM preservative_batches ORDER BY id`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int
		var code, brand, ing string
		var exp *time.Time
		var at time.Time
		if rows.Scan(&id, &code, &brand, &ing, &exp, &at) == nil {
			out = append(out, map[string]any{"id": id, "code": code, "brand": brand, "ingredient": ing,
				"expires_at": exp, "created_at": at})
		}
	}
	writeJSON(w, 200, out)
}

func (a *app) handleCreatePreservative(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code       string `json:"code"`
		Brand      string `json:"brand"`
		Ingredient string `json:"ingredient"`
		ExpiresAt  string `json:"expires_at"`
	}
	if err := decodeBody(r, &in); err != nil || in.Code == "" || in.Brand == "" {
		fail(w, 400, "批次号与品牌必填")
		return
	}
	var exp *time.Time
	if in.ExpiresAt != "" {
		if t, err := parseTime(in.ExpiresAt); err == nil {
			exp = &t
		}
	}
	var id int
	err := a.db.QueryRowContext(r.Context(),
		`INSERT INTO preservative_batches (code, brand, ingredient, expires_at) VALUES ($1,$2,$3,$4) RETURNING id`,
		in.Code, in.Brand, in.Ingredient, exp).Scan(&id)
	if err != nil {
		fail(w, 409, "创建失败（批次号可能重复）: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "code": in.Code})
}

// handleApplyPreservative 给花材批次施用保鲜剂，可售时长 +12 小时
func (a *app) handleApplyPreservative(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		PreservativeID int     `json:"preservative_id"`
		DosageML       float64 `json:"dosage_ml"`
	}
	if err := decodeBody(r, &in); err != nil || in.PreservativeID == 0 || in.DosageML <= 0 {
		fail(w, 400, "请指定保鲜剂与用量(ml)")
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
		`SELECT status FROM flower_batches WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		fail(w, 404, "批次不存在")
		return
	}
	if status == "expired" || status == "closed" {
		fail(w, 409, "批次已报损/出清，无法施用")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO batch_preservatives (batch_id, preservative_id, dosage_ml, applied_by) VALUES ($1,$2,$3,$4)`,
		id, in.PreservativeID, in.DosageML, u.ID); err != nil {
		fail(w, 500, "施用失败（保鲜剂不存在？）: "+err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE flower_batches SET sellable_hours=sellable_hours+12 WHERE id=$1`, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "sellable_hours_extended": 12})
}
