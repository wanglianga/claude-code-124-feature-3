package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// sweeper 后台清扫：结拍到期拍卖、取消超时未付款订单、标记账期逾期、可售期到报损
func (a *app) sweeper() {
	interval := time.Duration(a.cfg.SweepIntervalSec) * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		a.sweepOnce()
	}
}

func (a *app) sweepOnce() {
	ctx := context.Background()

	// 1) 到期拍卖自动结拍
	aucIDs := []int{}
	rows, err := a.db.Query(`SELECT id FROM auctions WHERE status='active' AND ends_at <= now()`)
	if err == nil {
		for rows.Next() {
			var id int
			if rows.Scan(&id) == nil {
				aucIDs = append(aucIDs, id)
			}
		}
		rows.Close()
	}
	for _, id := range aucIDs {
		tx, err := a.db.Begin()
		if err != nil {
			continue
		}
		if err := a.closeAuctionTx(tx, id); err != nil {
			tx.Rollback()
			log.Printf("拍卖 #%d 结拍失败: %v", id, err)
		} else {
			tx.Commit()
			log.Printf("拍卖 #%d 已自动结拍", id)
		}
	}

	// 2) 花店超时未付款 → 取消订单并释放锁定库存
	type pendingOrder struct{ id, invID, bundles int }
	pending := []pendingOrder{}
	rows, err = a.db.Query(
		`SELECT id, inventory_id, bundles FROM orders WHERE status='pending_payment' AND payment_deadline < now()`)
	if err == nil {
		for rows.Next() {
			var p pendingOrder
			if rows.Scan(&p.id, &p.invID, &p.bundles) == nil {
				pending = append(pending, p)
			}
		}
		rows.Close()
	}
	for _, p := range pending {
		tx, err := a.db.Begin()
		if err != nil {
			continue
		}
		res, err := tx.Exec(
			`UPDATE orders SET status='timeout_cancelled' WHERE id=$1 AND status='pending_payment'`, p.id)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				tx.Exec(`UPDATE inventories SET bundles_reserved=GREATEST(0,bundles_reserved-$1),
				         bundles_available=bundles_available+$1 WHERE id=$2`, p.bundles, p.invID)
				log.Printf("订单 #%d 超时未付款，已取消并释放 %d 扎", p.id, p.bundles)
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("订单 #%d 超时处理失败: %v", p.id, err)
		}
	}
	if len(pending) > 0 {
		a.rdb.Del(ctx, "cache:market")
	}

	// 3) 账期应收逾期标记
	if res, err := a.db.Exec(
		`UPDATE credit_receivables SET status='overdue' WHERE status='open' AND due_date < now()`); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("新增 %d 笔账期逾期", n)
		}
	}

	// 4) 可售时长到期且未售出 → 报损并释放冷库库位
	type expiring struct {
		id, bundles int
		slotID      *int
	}
	exp := []expiring{}
	rows, err = a.db.Query(
		`SELECT id, bundles, storage_slot_id FROM flower_batches
		 WHERE status IN ('qc_passed','stored')
		   AND COALESCE(stored_at, created_at) + make_interval(hours => sellable_hours) < now()`)
	if err == nil {
		for rows.Next() {
			var e expiring
			if rows.Scan(&e.id, &e.bundles, &e.slotID) == nil {
				exp = append(exp, e)
			}
		}
		rows.Close()
	}
	for _, e := range exp {
		tx, err := a.db.Begin()
		if err != nil {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE flower_batches SET status='expired', out_at=now() WHERE id=$1 AND status IN ('qc_passed','stored')`, e.id); err == nil {
			if e.slotID != nil {
				tx.Exec(`UPDATE storage_slots SET occupied_bundles=GREATEST(0, occupied_bundles-$1) WHERE id=$2`,
					e.bundles, *e.slotID)
			}
			// 仍在临时保温的批次随报损进入保温失败/责任确认流程
			tx.Exec(`UPDATE holding_records SET status='failed', failure_note='超过可售时长未售出，自动报损', failed_at=now()
			         WHERE batch_id=$1 AND status='active'`, e.id)
			log.Printf("批次 #%d 超过可售时长，已报损", e.id)
		}
		tx.Commit()
	}
	if len(exp) > 0 || len(aucIDs) > 0 {
		a.rdb.Del(ctx, "cache:overview")
	}

	// 5) 临时保温超时未入库 → 提醒冷库与市场管理方（每条保温记录只提醒一次）
	type holdingTimeout struct {
		id      int
		batchNo string
	}
	hts := []holdingTimeout{}
	rows, err = a.db.Query(
		`SELECT h.id, b.batch_no FROM holding_records h
		 JOIN flower_batches b ON b.id=h.batch_id
		 WHERE h.status='active' AND h.created_at + make_interval(hours => $1) < now()
		   AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.kind='holding_timeout' AND n.ref_id=h.id)`,
		a.cfg.HoldingMaxHours)
	if err == nil {
		for rows.Next() {
			var h holdingTimeout
			if rows.Scan(&h.id, &h.batchNo) == nil {
				hts = append(hts, h)
			}
		}
		rows.Close()
	}
	for _, h := range hts {
		tx, err := a.db.Begin()
		if err != nil {
			continue
		}
		title := fmt.Sprintf("临时保温超时：批次 %s 已超 %d 小时未入库", h.batchNo, a.cfg.HoldingMaxHours)
		body := "请尽快安排入库或检查保温状态，防范花材开放过快。"
		if err := notifyTx(ctx, tx, nil, "coldstore", "holding_timeout", h.id, title, body); err == nil {
			err = notifyTx(ctx, tx, nil, "manager", "holding_timeout", h.id, title, body)
		}
		if err != nil {
			tx.Rollback()
			log.Printf("保温超时提醒失败 #%d: %v", h.id, err)
			continue
		}
		tx.Commit()
		log.Printf("保温记录 #%d（批次 %s）超时未入库，已提醒冷库与管理方", h.id, h.batchNo)
	}
	if len(hts) > 0 {
		a.rdb.Del(ctx, "cache:overview")
	}
}
