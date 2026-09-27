package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// handleAnalyticsOverview 市场看板：成交速度 / 冷库占用 / 退货损耗 / 账期风险（Redis 缓存 10s）
func (a *app) handleAnalyticsOverview(w http.ResponseWriter, r *http.Request) {
	if cached, err := a.rdb.Get(r.Context(), "cache:overview").Result(); err == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(cached))
		return
	}
	ctx := r.Context()
	out := map[string]any{}

	// 成交速度：按品类+产地，从批发商上架到花店下单的平均分钟数、成交笔数
	dealSpeed := []map[string]any{}
	rows, err := a.db.QueryContext(ctx,
		`SELECT b.category, b.origin, count(o.id) AS deals,
		        round(avg(EXTRACT(EPOCH FROM (o.created_at - i.created_at))/60))::int AS avg_minutes,
		        round(sum(o.total_amount))::float8 AS amount
		 FROM orders o
		 JOIN inventories i ON i.id=o.inventory_id
		 JOIN flower_batches b ON b.id=o.batch_id
		 WHERE o.status IN ('paid','dispatched','delivered','completed')
		 GROUP BY 1,2 ORDER BY deals DESC, avg_minutes ASC LIMIT 50`)
	if err == nil {
		for rows.Next() {
			var cat, origin string
			var deals, avgMin int
			var amount float64
			if rows.Scan(&cat, &origin, &deals, &avgMin, &amount) == nil {
				dealSpeed = append(dealSpeed, map[string]any{"category": cat, "origin": origin,
					"deals": deals, "avg_minutes": avgMin, "amount": amount})
			}
		}
		rows.Close()
	}
	out["deal_speed"] = dealSpeed

	// 冷库占用
	slots := []map[string]any{}
	rows, err = a.db.QueryContext(ctx,
		`SELECT code, zone, capacity_bundles, occupied_bundles FROM storage_slots ORDER BY code`)
	if err == nil {
		for rows.Next() {
			var code, zone string
			var cap, occ int
			if rows.Scan(&code, &zone, &cap, &occ) == nil {
				slots = append(slots, map[string]any{"code": code, "zone": zone,
					"capacity_bundles": cap, "occupied_bundles": occ})
			}
		}
		rows.Close()
	}
	out["storage_slots"] = slots

	// 退货损耗：按品类+产地
	returnLoss := []map[string]any{}
	rows, err = a.db.QueryContext(ctx,
		`SELECT b.category, b.origin,
		        COALESCE(SUM(o.bundles),0) AS sold,
		        COALESCE(SUM(CASE WHEN r.status='approved' THEN r.bundles ELSE 0 END),0) AS returned,
		        COALESCE(SUM(CASE WHEN r.status='approved' THEN r.claim_amount ELSE 0 END),0) AS claim
		 FROM orders o
		 JOIN flower_batches b ON b.id=o.batch_id
		 LEFT JOIN returns r ON r.order_id=o.id
		 WHERE o.status IN ('paid','dispatched','delivered','completed')
		 GROUP BY 1,2 ORDER BY claim DESC LIMIT 50`)
	if err == nil {
		for rows.Next() {
			var cat, origin string
			var sold, returned int
			var claim float64
			if rows.Scan(&cat, &origin, &sold, &returned, &claim) == nil {
				rate := 0.0
				if sold > 0 {
					rate = round2(float64(returned) / float64(sold) * 100)
				}
				returnLoss = append(returnLoss, map[string]any{"category": cat, "origin": origin,
					"sold_bundles": sold, "returned_bundles": returned,
					"return_rate_pct": rate, "claim_amount": round2(claim)})
			}
		}
		rows.Close()
	}
	out["return_loss"] = returnLoss

	// 账期风险
	creditRisk := []map[string]any{}
	rows, err = a.db.QueryContext(ctx,
		`SELECT u.name, u.credit_limit,
		        COALESCE(SUM(CASE WHEN c.status='open' THEN c.amount END),0) AS open_amt,
		        COALESCE(SUM(CASE WHEN c.status='overdue' THEN c.amount END),0) AS overdue_amt,
		        COALESCE(MAX(CASE WHEN c.status='overdue' THEN EXTRACT(EPOCH FROM (now()-c.due_date))/86400 END),0)::int AS max_days
		 FROM users u
		 LEFT JOIN credit_receivables c ON c.customer_id=u.id AND c.status IN ('open','overdue')
		 WHERE u.role='shop' AND u.credit_days>0
		 GROUP BY u.id, u.name, u.credit_limit ORDER BY overdue_amt DESC`)
	if err == nil {
		for rows.Next() {
			var name string
			var limit, openAmt, overdueAmt float64
			var maxDays int
			if rows.Scan(&name, &limit, &openAmt, &overdueAmt, &maxDays) == nil {
				creditRisk = append(creditRisk, map[string]any{"customer": name, "credit_limit": limit,
					"open_amount": round2(openAmt), "overdue_amount": round2(overdueAmt), "max_overdue_days": maxDays})
			}
		}
		rows.Close()
	}
	out["credit_risk"] = creditRisk

	// 运营计数
	counts := map[string]int64{}
	for k, q := range map[string]string{
		"active_auctions":   `SELECT count(*) FROM auctions WHERE status='active'`,
		"pending_orders":    `SELECT count(*) FROM orders WHERE status='pending_payment'`,
		"timeout_orders":    `SELECT count(*) FROM orders WHERE status='timeout_cancelled'`,
		"requested_returns": `SELECT count(*) FROM returns WHERE status='requested'`,
		"expired_batches":   `SELECT count(*) FROM flower_batches WHERE status='expired'`,
		"stored_batches":    `SELECT count(*) FROM flower_batches WHERE status IN ('stored','in_auction')`,
		// 冷库库位不足调度：临时保温中、保温失败待双签、已双签定责、入场调整待确认/已确认
		"holding_active":              `SELECT count(*) FROM holding_records WHERE status='active'`,
		"holding_failed":              `SELECT count(*) FROM holding_records WHERE status='failed'`,
		"holding_pending_confirm":     `SELECT count(*) FROM holding_records WHERE status='failed' AND (coldstore_confirmed_by IS NULL OR wholesaler_confirmed_by IS NULL)`,
		"holding_confirmed":           `SELECT count(*) FROM holding_records WHERE final_responsibility<>''`,
		"entry_adjustments_pending":   `SELECT count(*) FROM entry_adjustments WHERE status='pending'`,
		"entry_adjustments_confirmed": `SELECT count(*) FROM entry_adjustments WHERE status='confirmed'`,
	} {
		var n int64
		if err := a.db.QueryRowContext(ctx, q).Scan(&n); err == nil {
			counts[k] = n
		}
	}
	out["counts"] = counts

	// 冷库爆满调度：当前临时保温与待处理责任/入场调整
	holdings := []map[string]any{}
	rows, err = a.db.QueryContext(ctx,
		`SELECT h.id, b.batch_no, b.category, b.sensitivity, h.method, h.responsibility, h.status,
		        h.coldstore_confirmed_by IS NOT NULL, h.wholesaler_confirmed_by IS NOT NULL, h.final_responsibility,
		        h.created_at, COALESCE(wu.name,'')
		 FROM holding_records h
		 JOIN flower_batches b ON b.id=h.batch_id
		 LEFT JOIN inventories i ON i.batch_id=b.id
		 LEFT JOIN users wu ON wu.id=i.wholesaler_id
		 WHERE h.status IN ('active','failed')
		 ORDER BY CASE h.status WHEN 'failed' THEN 0 ELSE 1 END, h.id DESC LIMIT 50`)
	if err == nil {
		for rows.Next() {
			var id int
			var batchNo, cat, sens, method, resp, status, finalResp, wholesaler string
			var csOK, wsOK bool
			var at time.Time
			if rows.Scan(&id, &batchNo, &cat, &sens, &method, &resp, &status, &csOK, &wsOK, &finalResp, &at, &wholesaler) == nil {
				holdings = append(holdings, map[string]any{"id": id, "batch_no": batchNo, "category": cat,
					"sensitivity": sens, "method": method, "responsibility": resp, "status": status,
					"coldstore_confirmed": csOK, "wholesaler_confirmed": wsOK,
					"final_responsibility": finalResp, "wholesaler_name": wholesaler, "created_at": at})
			}
		}
		rows.Close()
	}
	out["cold_holdings"] = holdings

	if data, err := json.Marshal(out); err == nil {
		a.rdb.Set(ctx, "cache:overview", data, 10*time.Second)
	}
	writeJSON(w, 200, out)
}

// handleAnalyticsStability 产地×品类稳定性：质检均分、冷链达标率、退货率 → 稳定性评分
func (a *app) handleAnalyticsStability(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type key struct{ origin, cat string }
	stats := map[key]*map[string]any{}

	rows, err := a.db.QueryContext(ctx,
		`SELECT b.origin, b.category,
		        count(DISTINCT b.id) AS batches,
		        COALESCE(avg(CASE q.grade WHEN 'A' THEN 4 WHEN 'B' THEN 3 WHEN 'C' THEN 2 WHEN 'D' THEN 1 END),0) AS grade_avg,
		        COALESCE(avg(CASE WHEN q.temp_ok THEN 1.0 ELSE 0.0 END),1) AS temp_ok_rate,
		        COALESCE(avg(CASE WHEN b.risk_flags='' THEN 1.0 ELSE 0.0 END),1) AS clean_rate
		 FROM flower_batches b
		 LEFT JOIN qc_records q ON q.batch_id=b.id
		 GROUP BY 1,2`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	for rows.Next() {
		var origin, cat string
		var batches int
		var gradeAvg, tempOK, clean float64
		if rows.Scan(&origin, &cat, &batches, &gradeAvg, &tempOK, &clean) == nil {
			stats[key{origin, cat}] = &map[string]any{
				"origin": origin, "category": cat, "batches": batches,
				"grade_avg": round2(gradeAvg), "temp_ok_rate": round2(tempOK * 100), "clean_rate": round2(clean * 100),
				"sold_bundles": 0, "returned_bundles": 0, "return_rate_pct": 0.0,
			}
		}
	}
	rows.Close()

	rows, err = a.db.QueryContext(ctx,
		`SELECT b.origin, b.category,
		        COALESCE(SUM(o.bundles),0), COALESCE(SUM(CASE WHEN r.status='approved' THEN r.bundles ELSE 0 END),0)
		 FROM orders o JOIN flower_batches b ON b.id=o.batch_id
		 LEFT JOIN returns r ON r.order_id=o.id
		 WHERE o.status IN ('paid','dispatched','delivered','completed')
		 GROUP BY 1,2`)
	if err == nil {
		for rows.Next() {
			var origin, cat string
			var sold, returned int
			if rows.Scan(&origin, &cat, &sold, &returned) == nil {
				if s, ok := stats[key{origin, cat}]; ok {
					(*s)["sold_bundles"] = sold
					(*s)["returned_bundles"] = returned
					if sold > 0 {
						(*s)["return_rate_pct"] = round2(float64(returned) / float64(sold) * 100)
					}
				}
			}
		}
		rows.Close()
	}

	out := []map[string]any{}
	for _, s := range stats {
		gradeAvg := (*s)["grade_avg"].(float64)
		tempOK := (*s)["temp_ok_rate"].(float64)
		retRate := (*s)["return_rate_pct"].(float64)
		// 稳定性评分：质检均分 60% + 冷链达标 20% + 低退货 20%
		score := gradeAvg/4*60 + tempOK/100*20 + (100-retRate)/100*20
		(*s)["stability_score"] = round2(score)
		out = append(out, *s)
	}
	// 按评分降序
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j]["stability_score"].(float64) > out[i]["stability_score"].(float64) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	writeJSON(w, 200, out)
}

// handleFarmerRanking 花农信用排名（Redis 有序集合，决定下次竞价排序）
func (a *app) handleFarmerRanking(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	zs, err := a.rdb.ZRevRangeWithScores(ctx, "farmer:credit", 0, 49).Result()
	if err != nil || len(zs) == 0 {
		a.syncFarmerRanking(ctx)
		zs, _ = a.rdb.ZRevRangeWithScores(ctx, "farmer:credit", 0, 49).Result()
	}
	names := map[int]string{}
	rows, err := a.db.QueryContext(ctx, `SELECT id, name FROM users WHERE role='farmer'`)
	if err == nil {
		for rows.Next() {
			var id int
			var name string
			if rows.Scan(&id, &name) == nil {
				names[id] = name
			}
		}
		rows.Close()
	}
	out := []map[string]any{}
	rank := 1
	for _, z := range zs {
		idStr, _ := z.Member.(string)
		fid, _ := strconv.Atoi(idStr)
		out = append(out, map[string]any{"rank": rank, "farmer_id": fid, "name": names[fid], "credit_score": int(z.Score)})
		rank++
	}
	writeJSON(w, 200, out)
}

// syncFarmerRanking 启动时把花农信用分同步到 Redis 有序集合
func (a *app) syncFarmerRanking(ctx context.Context) {
	rows, err := a.db.Query(`SELECT id, credit_score FROM users WHERE role='farmer'`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, score int
		if rows.Scan(&id, &score) == nil {
			a.rdb.ZAdd(ctx, "farmer:credit", redis.Z{Score: float64(score), Member: id})
		}
	}
}
