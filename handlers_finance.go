package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// ---------- 账期应收 ----------

func (a *app) handleListReceivables(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT c.id, c.amount, c.due_date, c.status, c.settled_at, c.created_at,
		        u.name, COALESCE(o.order_no,''), c.order_id
		 FROM credit_receivables c
		 JOIN users u ON u.id=c.customer_id
		 JOIN orders o ON o.id=c.order_id
		 ORDER BY c.status='overdue' DESC, c.due_date DESC LIMIT 200`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	now := time.Now()
	for rows.Next() {
		var (
			id, orderID           int
			amount                float64
			due, createdAt        time.Time
			status, name, orderNo string
			settledAt             *time.Time
		)
		if rows.Scan(&id, &amount, &due, &status, &settledAt, &createdAt, &name, &orderNo, &orderID) == nil {
			daysOverdue := 0
			if status == "overdue" {
				daysOverdue = int(now.Sub(due).Hours() / 24)
			}
			out = append(out, map[string]any{
				"id": id, "order_id": orderID, "order_no": orderNo, "customer": name,
				"amount": amount, "due_date": due, "status": status,
				"settled_at": settledAt, "created_at": createdAt, "days_overdue": daysOverdue,
			})
		}
	}
	writeJSON(w, 200, out)
}

// handleSettleReceivable 财务登记账期回款（线下核销）
func (a *app) handleSettleReceivable(w http.ResponseWriter, r *http.Request) {
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
		status  string
		orderID int
		amount  float64
	)
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, order_id, amount FROM credit_receivables WHERE id=$1 FOR UPDATE`, id).
		Scan(&status, &orderID, &amount)
	if err == sql.ErrNoRows {
		fail(w, 404, "应收记录不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status == "settled" {
		fail(w, 409, "该应收已核销")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE credit_receivables SET status='settled', settled_at=now() WHERE id=$1`, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO payments (order_id, amount, method, recorded_by) VALUES ($1,$2,'offline',$3)`,
		orderID, amount, u.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 200, map[string]any{"id": id, "status": "settled", "amount": amount})
}

// ---------- 月度结算 ----------

type settleLine struct {
	kind, refType, desc string
	refID               int
	amount              float64 // 正=收入/应付，负=扣减/返还
}

// handleGenerateSettlement 按成交价、损耗比例、冷库时长、退货原因、账期扣款生成月度结算
func (a *app) handleGenerateSettlement(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		Month string `json:"month"` // YYYY-MM
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请给出 month，如 2026-09")
		return
	}
	start, err := time.Parse("2006-01", in.Month)
	if err != nil {
		fail(w, 400, "month 格式须为 YYYY-MM")
		return
	}
	end := start.AddDate(0, 1, 0)
	now := time.Now()
	rate := a.cfg.StorageFeePerBundleHour
	penaltyRate := a.cfg.CreditDailyPenaltyRate

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM settlements WHERE month=$1`, in.Month); err != nil {
		fail(w, 500, err.Error())
		return
	}

	insertSettlement := func(role string, entityID int, gross, loss, storage, credit, net float64, lines []settleLine) error {
		var sid int
		err := tx.QueryRowContext(r.Context(),
			`INSERT INTO settlements (month, entity_role, entity_id, gross_amount, loss_deduction, storage_fee, credit_deduction, net_amount, generated_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
			in.Month, role, entityID, round2(gross), round2(loss), round2(storage), round2(credit), round2(net), u.ID).Scan(&sid)
		if err != nil {
			return err
		}
		for _, l := range lines {
			if _, err := tx.ExecContext(r.Context(),
				`INSERT INTO settlement_lines (settlement_id, kind, ref_type, ref_id, description, amount)
				 VALUES ($1,$2,$3,$4,$5,$6)`, sid, l.kind, l.refType, l.refID, l.desc, round2(l.amount)); err != nil {
				return err
			}
		}
		return nil
	}

	// 冷库费：入库时长 × 扎数 × 费率
	storageFeeOf := func(storedAt time.Time, outAt *time.Time, bundles int) float64 {
		endT := now
		if outAt != nil && outAt.Before(now) {
			endT = *outAt
		}
		hours := endT.Sub(storedAt).Hours()
		if hours < 0 {
			hours = 0
		}
		return hours * float64(bundles) * rate
	}

	// ---- 花农：成交收入 − 退货扣款 − 冷库费（未售出批次） ----
	farmerIDs, err := a.intColumn(r, `SELECT id FROM users WHERE role='farmer'`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	for _, fid := range farmerIDs {
		lines := []settleLine{}
		gross, loss, storage := 0.0, 0.0, 0.0
		rows, err := tx.QueryContext(r.Context(),
			`SELECT a.id, b.batch_no, a.current_price, b.bundles FROM auctions a
			 JOIN flower_batches b ON b.id=a.batch_id
			 WHERE b.farmer_id=$1 AND a.status='closed' AND a.ends_at>=$2 AND a.ends_at<$3`, fid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var aid, bundles int
			var no string
			var price float64
			if rows.Scan(&aid, &no, &price, &bundles) == nil {
				amt := price * float64(bundles)
				gross += amt
				lines = append(lines, settleLine{"deal_income", "auction", fmt.Sprintf("批次 %s 竞价成交", no), aid, amt})
			}
		}
		rows.Close()
		rows, err = tx.QueryContext(r.Context(),
			`SELECT r.id, r.claim_amount, b.batch_no, r.reason FROM returns r
			 JOIN flower_batches b ON b.id=r.batch_id
			 WHERE b.farmer_id=$1 AND r.status='approved' AND r.farmer_responsible
			   AND r.reviewed_at>=$2 AND r.reviewed_at<$3`, fid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var rid int
			var claim float64
			var no, reason string
			if rows.Scan(&rid, &claim, &no, &reason) == nil {
				loss += claim
				lines = append(lines, settleLine{"return_deduction", "return",
					fmt.Sprintf("批次 %s 退货扣款（%s）", no, reason), rid, -claim})
			}
		}
		rows.Close()
		// 临时保温失败双签定责：判定花农责任的损耗赔付扣款
		rows, err = tx.QueryContext(r.Context(),
			`SELECT h.id, b.batch_no, h.compensation, h.failed_at FROM holding_records h
			 JOIN flower_batches b ON b.id=h.batch_id
			 WHERE b.farmer_id=$1 AND h.final_responsibility='farmer' AND h.compensation>0
			   AND h.failed_at>=$2 AND h.failed_at<$3`, fid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var hid int
			var no string
			var comp float64
			var failedAt time.Time
			if rows.Scan(&hid, &no, &comp, &failedAt) == nil {
				loss += comp
				lines = append(lines, settleLine{"holding_loss", "holding",
					fmt.Sprintf("批次 %s 临时保温失败花农责任赔付（开放过快）", no), hid, -comp})
			}
		}
		rows.Close()
		rows, err = tx.QueryContext(r.Context(),
			`SELECT b.id, b.batch_no, b.bundles, b.stored_at, b.out_at FROM flower_batches b
			 WHERE b.farmer_id=$1 AND b.stored_at IS NOT NULL AND b.stored_at<$2
			   AND NOT EXISTS (SELECT 1 FROM inventories i WHERE i.batch_id=b.id)`, fid, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var bid, bundles int
			var no string
			var storedAt time.Time
			var outAt *time.Time
			if rows.Scan(&bid, &no, &bundles, &storedAt, &outAt) == nil {
				fee := storageFeeOf(storedAt, outAt, bundles)
				if fee > 0 {
					storage += fee
					lines = append(lines, settleLine{"storage_fee", "batch",
						fmt.Sprintf("批次 %s 冷库费（%d 扎）", no, bundles), bid, -fee})
				}
			}
		}
		rows.Close()
		if len(lines) == 0 {
			continue
		}
		if err := insertSettlement("farmer", fid, gross, loss, storage, 0, gross-loss-storage, lines); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}

	// ---- 批发商：销售净成交（销售−采购）− 退货赔付 − 冷库费 ----
	wholesalerIDs, err := a.intColumn(r, `SELECT id FROM users WHERE role='wholesaler'`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	for _, wid := range wholesalerIDs {
		lines := []settleLine{}
		gross, loss, storage := 0.0, 0.0, 0.0
		rows, err := tx.QueryContext(r.Context(),
			`SELECT o.id, o.order_no, o.total_amount FROM orders o
			 WHERE o.wholesaler_id=$1 AND o.paid_at>=$2 AND o.paid_at<$3
			   AND o.status IN ('paid','dispatched','delivered','completed')`, wid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var oid int
			var no string
			var amt float64
			if rows.Scan(&oid, &no, &amt) == nil {
				gross += amt
				lines = append(lines, settleLine{"sale_income", "order", fmt.Sprintf("订单 %s 销售成交", no), oid, amt})
			}
		}
		rows.Close()
		rows, err = tx.QueryContext(r.Context(),
			`SELECT a.id, b.batch_no, a.current_price, b.bundles FROM auctions a
			 JOIN flower_batches b ON b.id=a.batch_id
			 WHERE a.winner_id=$1 AND a.status='closed' AND a.ends_at>=$2 AND a.ends_at<$3`, wid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var aid, bundles int
			var no string
			var price float64
			if rows.Scan(&aid, &no, &price, &bundles) == nil {
				amt := price * float64(bundles)
				gross -= amt
				lines = append(lines, settleLine{"purchase_cost", "auction",
					fmt.Sprintf("批次 %s 竞价采购成本", no), aid, -amt})
			}
		}
		rows.Close()
		rows, err = tx.QueryContext(r.Context(),
			`SELECT r.id, r.claim_amount, o.order_no, r.reason FROM returns r
			 JOIN orders o ON o.id=r.order_id
			 WHERE o.wholesaler_id=$1 AND r.status='approved' AND r.reviewed_at>=$2 AND r.reviewed_at<$3`, wid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var rid int
			var claim float64
			var no, reason string
			if rows.Scan(&rid, &claim, &no, &reason) == nil {
				loss += claim
				lines = append(lines, settleLine{"return_refund", "return",
					fmt.Sprintf("订单 %s 退货赔付（%s）", no, reason), rid, -claim})
			}
		}
		rows.Close()
		// 临时保温失败双签定责：判定批发商责任的损耗赔付（如冰袋耗尽致开放过快）
		rows, err = tx.QueryContext(r.Context(),
			`SELECT h.id, b.batch_no, h.compensation, h.failed_at FROM holding_records h
			 JOIN flower_batches b ON b.id=h.batch_id
			 JOIN inventories iv ON iv.batch_id=b.id
			 WHERE iv.wholesaler_id=$1 AND h.final_responsibility='wholesaler' AND h.compensation>0
			   AND h.failed_at>=$2 AND h.failed_at<$3`, wid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var hid int
			var no string
			var comp float64
			var failedAt time.Time
			if rows.Scan(&hid, &no, &comp, &failedAt) == nil {
				loss += comp
				lines = append(lines, settleLine{"holding_loss", "holding",
					fmt.Sprintf("批次 %s 临时保温失败批发商责任赔付（开放过快）", no), hid, -comp})
			}
		}
		rows.Close()
		rows, err = tx.QueryContext(r.Context(),
			`SELECT b.id, b.batch_no, b.bundles, b.stored_at, b.out_at FROM flower_batches b
			 JOIN inventories i ON i.batch_id=b.id
			 WHERE i.wholesaler_id=$1 AND b.stored_at IS NOT NULL AND b.stored_at<$2`, wid, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var bid, bundles int
			var no string
			var storedAt time.Time
			var outAt *time.Time
			if rows.Scan(&bid, &no, &bundles, &storedAt, &outAt) == nil {
				fee := storageFeeOf(storedAt, outAt, bundles)
				if fee > 0 {
					storage += fee
					lines = append(lines, settleLine{"storage_fee", "batch",
						fmt.Sprintf("批次 %s 冷库费（%d 扎）", no, bundles), bid, -fee})
				}
			}
		}
		rows.Close()
		if len(lines) == 0 {
			continue
		}
		if err := insertSettlement("wholesaler", wid, gross, loss, storage, 0, gross-loss-storage, lines); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}

	// ---- 花店/婚庆：采购应付 − 退货返还 + 账期逾期罚息 ----
	shopIDs, err := a.intColumn(r, `SELECT id FROM users WHERE role='shop'`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	for _, sid := range shopIDs {
		lines := []settleLine{}
		gross, loss, credit := 0.0, 0.0, 0.0
		rows, err := tx.QueryContext(r.Context(),
			`SELECT o.id, o.order_no, o.total_amount FROM orders o
			 WHERE o.shop_id=$1 AND o.paid_at>=$2 AND o.paid_at<$3
			   AND o.status IN ('paid','dispatched','delivered','completed')`, sid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var oid int
			var no string
			var amt float64
			if rows.Scan(&oid, &no, &amt) == nil {
				gross += amt
				lines = append(lines, settleLine{"purchase", "order", fmt.Sprintf("订单 %s 采购", no), oid, amt})
			}
		}
		rows.Close()
		rows, err = tx.QueryContext(r.Context(),
			`SELECT r.id, r.claim_amount, o.order_no, r.reason FROM returns r
			 JOIN orders o ON o.id=r.order_id
			 WHERE r.shop_id=$1 AND r.status='approved' AND r.reviewed_at>=$2 AND r.reviewed_at<$3`, sid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var rid int
			var claim float64
			var no, reason string
			if rows.Scan(&rid, &claim, &no, &reason) == nil {
				loss += claim
				lines = append(lines, settleLine{"return_refund", "return",
					fmt.Sprintf("订单 %s 退货返还（%s）", no, reason), rid, -claim})
			}
		}
		rows.Close()
		rows, err = tx.QueryContext(r.Context(),
			`SELECT c.id, c.amount, c.due_date, c.settled_at, c.status FROM credit_receivables c
			 WHERE c.customer_id=$1 AND c.due_date>=$2 AND c.due_date<$3
			   AND (c.status='overdue' OR (c.status='settled' AND c.settled_at>c.due_date))`, sid, start, end)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var cid int
			var amount float64
			var due time.Time
			var settledAt *time.Time
			var status string
			if rows.Scan(&cid, &amount, &due, &settledAt, &status) == nil {
				endT := now
				if settledAt != nil {
					endT = *settledAt
				}
				days := int(endT.Sub(due).Hours() / 24)
				if days < 1 {
					days = 1
				}
				penalty := amount * penaltyRate * float64(days)
				if cap := amount * 0.2; penalty > cap {
					penalty = cap
				}
				credit += penalty
				lines = append(lines, settleLine{"credit_penalty", "receivable",
					fmt.Sprintf("账期逾期 %d 天罚息", days), cid, penalty})
			}
		}
		rows.Close()
		if len(lines) == 0 {
			continue
		}
		// 花店为付款方：净额 = 采购 − 退货返还 + 罚息
		if err := insertSettlement("shop", sid, gross, loss, 0, credit, gross-loss+credit, lines); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}

	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	var count int
	a.db.QueryRowContext(r.Context(), `SELECT count(*) FROM settlements WHERE month=$1`, in.Month).Scan(&count)
	writeJSON(w, 201, map[string]any{"month": in.Month, "settlements": count})
}

func (a *app) intColumn(r *http.Request, q string, args ...any) ([]int, error) {
	rows, err := a.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var id int
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out, nil
}

// handleListSettlements 结算单列表（各主体只能看自己的，财务/管理看全部）
func (a *app) handleListSettlements(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT s.id, s.month, s.entity_role, s.entity_id, s.gross_amount, s.loss_deduction,
	             s.storage_fee, s.credit_deduction, s.net_amount, s.created_at, u.name
	      FROM settlements s JOIN users u ON u.id=s.entity_id`
	args := []any{}
	where := ""
	if u.Role == "farmer" || u.Role == "wholesaler" || u.Role == "shop" {
		where = " WHERE s.entity_id=$1"
		args = append(args, u.ID)
	}
	if m := r.URL.Query().Get("month"); m != "" {
		if where == "" {
			where = fmt.Sprintf(" WHERE s.month=$%d", len(args)+1)
		} else {
			where += fmt.Sprintf(" AND s.month=$%d", len(args)+1)
		}
		args = append(args, m)
	}
	rows, err := a.db.QueryContext(r.Context(), q+where+" ORDER BY s.month DESC, s.entity_role, s.id", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id, entityID                      int
			month, role, name                 string
			gross, loss, storage, credit, net float64
			at                                time.Time
		)
		if rows.Scan(&id, &month, &role, &entityID, &gross, &loss, &storage, &credit, &net, &at, &name) == nil {
			out = append(out, map[string]any{
				"id": id, "month": month, "entity_role": role, "entity_id": entityID, "entity_name": name,
				"gross_amount": gross, "loss_deduction": loss, "storage_fee": storage,
				"credit_deduction": credit, "net_amount": net, "created_at": at,
			})
		}
	}
	writeJSON(w, 200, out)
}

func (a *app) handleGetSettlement(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var (
		month, role, name                 string
		entityID                          int
		gross, loss, storage, credit, net float64
		at                                time.Time
	)
	err := a.db.QueryRowContext(r.Context(),
		`SELECT s.month, s.entity_role, s.entity_id, s.gross_amount, s.loss_deduction, s.storage_fee,
		        s.credit_deduction, s.net_amount, s.created_at, u.name
		 FROM settlements s JOIN users u ON u.id=s.entity_id WHERE s.id=$1`, id).
		Scan(&month, &role, &entityID, &gross, &loss, &storage, &credit, &net, &at, &name)
	if err == sql.ErrNoRows {
		fail(w, 404, "结算单不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if (u.Role == "farmer" || u.Role == "wholesaler" || u.Role == "shop") && u.ID != entityID {
		fail(w, 403, "只能查看自己的结算单")
		return
	}
	lines := []map[string]any{}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT kind, ref_type, ref_id, description, amount FROM settlement_lines WHERE settlement_id=$1 ORDER BY id`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var kind, refType, desc string
			var refID int
			var amount float64
			if rows.Scan(&kind, &refType, &refID, &desc, &amount) == nil {
				lines = append(lines, map[string]any{"kind": kind, "ref_type": refType, "ref_id": refID,
					"description": desc, "amount": amount})
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "month": month, "entity_role": role, "entity_id": entityID, "entity_name": name,
		"gross_amount": gross, "loss_deduction": loss, "storage_fee": storage,
		"credit_deduction": credit, "net_amount": net, "created_at": at, "lines": lines,
	})
}
