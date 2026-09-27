package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ---------- 凌晨竞价 / 一口价 ----------

// handleCreateAuction 管理方为质检通过的批次发起竞价或一口价
func (a *app) handleCreateAuction(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		BatchID           int      `json:"batch_id"`
		Mode              string   `json:"mode"` // auction / fixed
		StartPrice        float64  `json:"start_price"`
		BuyNowPrice       *float64 `json:"buy_now_price"`
		MinIncrement      *float64 `json:"min_increment"`
		DurationMin       *int     `json:"duration_min"`
		EstimatedPickupAt string   `json:"estimated_pickup_at"` // 预计提货时间（成交后回写批次，影响爆满时入库优先级）
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.Mode != "auction" && in.Mode != "fixed" {
		fail(w, 400, "mode 须为 auction(竞价) 或 fixed(一口价)")
		return
	}
	if in.StartPrice <= 0 {
		fail(w, 400, "起拍价须大于 0")
		return
	}
	if in.Mode == "fixed" && (in.BuyNowPrice == nil || *in.BuyNowPrice <= 0) {
		fail(w, 400, "一口价模式必须给出 buy_now_price")
		return
	}
	var estimatedPickup *time.Time
	if in.EstimatedPickupAt != "" {
		t, err := parseTime(in.EstimatedPickupAt)
		if err != nil {
			fail(w, 400, "预计提货时间格式错误，示例 2026-09-27T08:30")
			return
		}
		estimatedPickup = &t
	}
	inc := 1.0
	if in.MinIncrement != nil && *in.MinIncrement > 0 {
		inc = *in.MinIncrement
	}
	dur := 30
	if in.DurationMin != nil && *in.DurationMin > 0 {
		dur = *in.DurationMin
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var status, grade string
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, grade FROM flower_batches WHERE id=$1 FOR UPDATE`, in.BatchID).Scan(&status, &grade)
	if err == sql.ErrNoRows {
		fail(w, 404, "批次不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if grade == "D" {
		fail(w, 409, "质检 D 级批次不得上拍")
		return
	}
	if status != "qc_passed" && status != "stored" {
		fail(w, 409, "批次当前状态不可上拍: "+status)
		return
	}
	var auctionID int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO auctions (batch_id, mode, start_price, buy_now_price, min_increment, ends_at, created_by, estimated_pickup_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		in.BatchID, in.Mode, in.StartPrice, in.BuyNowPrice, inc,
		time.Now().Add(time.Duration(dur)*time.Minute), u.ID, estimatedPickup).Scan(&auctionID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE flower_batches SET status='in_auction' WHERE id=$1`, in.BatchID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:overview")
	writeJSON(w, 201, map[string]any{"id": auctionID, "batch_id": in.BatchID, "mode": in.Mode})
}

// handleListAuctions 竞价大厅：按花农信用分排序（退货判定影响下次竞价排序）
func (a *app) handleListAuctions(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT a.id, a.mode, a.start_price, a.buy_now_price, a.min_increment, a.current_price,
		        a.status, a.starts_at, a.ends_at, a.winner_id, a.estimated_pickup_at,
		        b.id, b.batch_no, b.category, b.origin, b.grade, b.bundles, b.openness, b.risk_flags,
		        f.name, f.credit_score,
		        (SELECT count(*) FROM bids WHERE auction_id=a.id)
		 FROM auctions a
		 JOIN flower_batches b ON b.id=a.batch_id
		 JOIN users f ON f.id=b.farmer_id
		 ORDER BY CASE WHEN a.status='active' THEN 0 ELSE 1 END,
		          f.credit_score DESC, a.ends_at ASC
		 LIMIT 100`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			aid, bidCount, batchID, bundles, credit                  int
			mode, status, batchNo, cat, origin, grade, flags, farmer string
			startPrice, minInc, open                                 float64
			buyNow, cur                                              *float64
			winner                                                   *int
			startsAt, endsAt                                         time.Time
			estimatedPickup                                          *time.Time
		)
		if err := rows.Scan(&aid, &mode, &startPrice, &buyNow, &minInc, &cur, &status, &startsAt, &endsAt,
			&winner, &estimatedPickup, &batchID, &batchNo, &cat, &origin, &grade, &bundles, &open, &flags,
			&farmer, &credit, &bidCount); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": aid, "mode": mode, "start_price": startPrice, "buy_now_price": buyNow,
			"min_increment": minInc, "current_price": cur, "status": status,
			"starts_at": startsAt, "ends_at": endsAt, "winner_id": winner, "bid_count": bidCount,
			"estimated_pickup_at": estimatedPickup,
			"batch": map[string]any{"id": batchID, "batch_no": batchNo, "category": cat, "origin": origin,
				"grade": grade, "bundles": bundles, "openness": open, "risk_flags": flags},
			"farmer_name": farmer, "farmer_credit": credit,
		})
	}
	writeJSON(w, 200, out)
}

func (a *app) handleGetAuction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var (
		mode, status, batchNo, cat, origin, farmer string
		startPrice, minInc                         float64
		buyNow, cur                                *float64
		winner                                     *int
		startsAt, endsAt                           time.Time
		estimatedPickup                            *time.Time
		credit                                     int
	)
	err := a.db.QueryRowContext(r.Context(),
		`SELECT a.mode, a.start_price, a.buy_now_price, a.min_increment, a.current_price, a.status,
		        a.starts_at, a.ends_at, a.winner_id, a.estimated_pickup_at,
		        b.batch_no, b.category, b.origin, f.name, f.credit_score
		 FROM auctions a JOIN flower_batches b ON b.id=a.batch_id JOIN users f ON f.id=b.farmer_id
		 WHERE a.id=$1`, id).
		Scan(&mode, &startPrice, &buyNow, &minInc, &cur, &status, &startsAt, &endsAt, &winner,
			&estimatedPickup, &batchNo, &cat, &origin, &farmer, &credit)
	if err == sql.ErrNoRows {
		fail(w, 404, "拍卖不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	bids := []map[string]any{}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT b.id, b.price, b.created_at, u.name FROM bids b JOIN users u ON u.id=b.wholesaler_id
		 WHERE b.auction_id=$1 ORDER BY b.price DESC, b.id LIMIT 20`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var bidID int
			var price float64
			var at time.Time
			var name string
			if rows.Scan(&bidID, &price, &at, &name) == nil {
				bids = append(bids, map[string]any{"id": bidID, "price": price, "created_at": at, "wholesaler": name})
			}
		}
	}
	// Redis 实时最高价缓存（出价时写入）
	rtTop, _ := a.rdb.HGetAll(r.Context(), fmt.Sprintf("auction:top:%d", id)).Result()
	writeJSON(w, 200, map[string]any{
		"id": id, "mode": mode, "start_price": startPrice, "buy_now_price": buyNow,
		"min_increment": minInc, "current_price": cur, "status": status,
		"starts_at": startsAt, "ends_at": endsAt, "winner_id": winner,
		"estimated_pickup_at": estimatedPickup,
		"batch_no":            batchNo, "category": cat, "origin": origin,
		"farmer_name": farmer, "farmer_credit": credit,
		"bids": bids, "realtime_top": rtTop,
	})
}

// handleBid 批发商出价（行锁保证不超价、Redis 记录实时最高价）
func (a *app) handleBid(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Price float64 `json:"price"`
	}
	if err := decodeBody(r, &in); err != nil || in.Price <= 0 {
		fail(w, 400, "请给出有效出价(每扎)")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var mode, status string
	var startPrice, minInc float64
	var cur *float64
	var endsAt time.Time
	err = tx.QueryRowContext(r.Context(),
		`SELECT mode, status, start_price, min_increment, current_price, ends_at
		 FROM auctions WHERE id=$1 FOR UPDATE`, id).
		Scan(&mode, &status, &startPrice, &minInc, &cur, &endsAt)
	if err == sql.ErrNoRows {
		fail(w, 404, "拍卖不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "active" {
		fail(w, 409, "拍卖已结束")
		return
	}
	if mode != "auction" {
		fail(w, 409, "一口价专场请直接购买")
		return
	}
	if time.Now().After(endsAt) {
		fail(w, 409, "拍卖已到截止时间，等待结拍")
		return
	}
	minAllowed := startPrice
	if cur != nil {
		minAllowed = *cur + minInc
	}
	if in.Price < minAllowed {
		fail(w, 409, fmt.Sprintf("出价须不低于 %.2f 元/扎", minAllowed))
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO bids (auction_id, wholesaler_id, price) VALUES ($1,$2,$3)`, id, u.ID, in.Price); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE auctions SET current_price=$1 WHERE id=$2`, in.Price, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	key := fmt.Sprintf("auction:top:%d", id)
	a.rdb.HSet(r.Context(), key, "price", in.Price, "wholesaler", u.Name)
	a.rdb.Expire(r.Context(), key, 2*time.Hour)
	writeJSON(w, 200, map[string]any{"auction_id": id, "current_price": in.Price})
}

// handleBuyNow 一口价直接成交
func (a *app) handleBuyNow(w http.ResponseWriter, r *http.Request) {
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
	var mode, status string
	var buyNow *float64
	err = tx.QueryRowContext(r.Context(),
		`SELECT mode, status, buy_now_price FROM auctions WHERE id=$1 FOR UPDATE`, id).
		Scan(&mode, &status, &buyNow)
	if err == sql.ErrNoRows {
		fail(w, 404, "拍卖不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if status != "active" {
		fail(w, 409, "该场次已结束")
		return
	}
	if mode != "fixed" || buyNow == nil {
		fail(w, 409, "该场次不支持一口价")
		return
	}
	// 留一笔出价记录便于对账
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO bids (auction_id, wholesaler_id, price) VALUES ($1,$2,$3)`, id, u.ID, *buyNow); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := a.closeAuctionTx(tx, id); err != nil {
		fail(w, 500, "成交失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:market", "cache:overview")
	writeJSON(w, 200, map[string]any{"auction_id": id, "status": "closed", "price": *buyNow, "winner_id": u.ID})
}

// handleCloseAuction 管理方手动结拍（清扫器也会自动结拍到期的）
func (a *app) handleCloseAuction(w http.ResponseWriter, r *http.Request) {
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
	if err := a.closeAuctionTx(tx, id); err != nil {
		fail(w, 409, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:market", "cache:overview")
	writeJSON(w, 200, map[string]any{"auction_id": id, "status": "closed"})
}

// closeAuctionTx 结拍：最高价者得，生成批发商库存；无出价则流拍退回
func (a *app) closeAuctionTx(tx *sql.Tx, auctionID int) error {
	var batchID int
	var status string
	err := tx.QueryRow(`SELECT batch_id, status FROM auctions WHERE id=$1 FOR UPDATE`, auctionID).
		Scan(&batchID, &status)
	if err != nil {
		return fmt.Errorf("拍卖不存在")
	}
	if status != "active" {
		return fmt.Errorf("拍卖已结拍，状态: %s", status)
	}
	var winnerID int
	var top float64
	err = tx.QueryRow(
		`SELECT wholesaler_id, price FROM bids WHERE auction_id=$1 ORDER BY price DESC, id ASC LIMIT 1`,
		auctionID).Scan(&winnerID, &top)
	if err == sql.ErrNoRows {
		if _, err := tx.Exec(`UPDATE auctions SET status='unsold' WHERE id=$1`, auctionID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE flower_batches SET status='qc_passed' WHERE id=$1`, batchID)
		return err
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE auctions SET status='closed', winner_id=$1, current_price=$2 WHERE id=$3`,
		winnerID, top, auctionID); err != nil {
		return err
	}
	// 成交后把场次的预计提货时间回写批次（批次已登记过则保留），作为爆满时优先入库依据
	if _, err := tx.Exec(`UPDATE flower_batches SET status='sold',
		expected_pickup_at=COALESCE(expected_pickup_at, (SELECT estimated_pickup_at FROM auctions WHERE id=$1))
		WHERE id=$2`, auctionID, batchID); err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO inventories (wholesaler_id, batch_id, auction_id, bundles_total, bundles_available, cost_price)
		 SELECT $1, b.id, $2, b.bundles, b.bundles, $3 FROM flower_batches b WHERE b.id=$4`,
		winnerID, auctionID, top, batchID)
	return err
}

// ---------- 批发商库存 / 调价 ----------

func (a *app) inventoryRows(r *http.Request, where string, args ...any) ([]map[string]any, error) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT i.id, i.bundles_total, i.bundles_available, i.bundles_reserved, i.cost_price, i.list_price,
		        i.listed, i.status, i.created_at,
		        b.id, b.batch_no, b.category, b.origin, b.grade, b.risk_flags, u.name
		 FROM inventories i
		 JOIN flower_batches b ON b.id=i.batch_id
		 JOIN users u ON u.id=i.wholesaler_id `+where+` ORDER BY i.id DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			iid, total, avail, reserved, batchID          int
			cost                                          float64
			listPrice                                     *float64
			listed                                        bool
			status, batchNo, cat, origin, grade, flags, w string
			createdAt                                     time.Time
		)
		if err := rows.Scan(&iid, &total, &avail, &reserved, &cost, &listPrice, &listed, &status,
			&createdAt, &batchID, &batchNo, &cat, &origin, &grade, &flags, &w); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": iid, "bundles_total": total, "bundles_available": avail, "bundles_reserved": reserved,
			"cost_price": cost, "list_price": listPrice, "listed": listed, "status": status, "created_at": createdAt,
			"batch": map[string]any{"id": batchID, "batch_no": batchNo, "category": cat, "origin": origin,
				"grade": grade, "risk_flags": flags},
			"wholesaler_name": w,
		})
	}
	return out, nil
}

// handleListInventories 批发商查看自己的库存；管理/财务看全部
func (a *app) handleListInventories(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var (
		out []map[string]any
		err error
	)
	if u.Role == "wholesaler" {
		out, err = a.inventoryRows(r, "WHERE i.wholesaler_id=$1", u.ID)
	} else {
		out, err = a.inventoryRows(r, "")
	}
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

// handleListMarket 花店采购市场：已上架且有余量（Redis 缓存 5s）
func (a *app) handleListMarket(w http.ResponseWriter, r *http.Request) {
	if cached, err := a.rdb.Get(r.Context(), "cache:market").Result(); err == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(cached))
		return
	}
	out, err := a.inventoryRows(r, "WHERE i.listed AND i.bundles_available>0 AND i.status='in_stock'")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if data, err := json.Marshal(out); err == nil {
		a.rdb.Set(r.Context(), "cache:market", data, 5*time.Second)
	}
	writeJSON(w, 200, out)
}

// handleSetListing 上架/下架
func (a *app) handleSetListing(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		Listed    bool     `json:"listed"`
		ListPrice *float64 `json:"list_price"`
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
	var owner int
	var listPrice *float64
	err = tx.QueryRowContext(r.Context(),
		`SELECT wholesaler_id, list_price FROM inventories WHERE id=$1 FOR UPDATE`, id).Scan(&owner, &listPrice)
	if err == sql.ErrNoRows {
		fail(w, 404, "库存不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if owner != u.ID {
		fail(w, 403, "只能操作自己的库存")
		return
	}
	if in.ListPrice != nil {
		if *in.ListPrice <= 0 {
			fail(w, 400, "挂牌价须大于 0")
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO price_changes (inventory_id, old_price, new_price, reason, changed_by) VALUES ($1,$2,$3,'上架定价',$4)`,
			id, listPrice, *in.ListPrice, u.ID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE inventories SET list_price=$1 WHERE id=$2`, *in.ListPrice, id); err != nil {
			fail(w, 500, err.Error())
			return
		}
	} else if in.Listed && listPrice == nil {
		fail(w, 409, "上架前请给出挂牌价 list_price")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE inventories SET listed=$1 WHERE id=$2`, in.Listed, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:market")
	writeJSON(w, 200, map[string]any{"id": id, "listed": in.Listed})
}

// handleChangePrice 批发商临时改价：留痕 price_changes，未付款订单仍按下单时锁定价
func (a *app) handleChangePrice(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, ok := pathID(r, "id")
	if !ok {
		fail(w, 400, "id 错误")
		return
	}
	var in struct {
		NewPrice float64 `json:"new_price"`
		Reason   string  `json:"reason"`
	}
	if err := decodeBody(r, &in); err != nil || in.NewPrice <= 0 {
		fail(w, 400, "请给出有效新价格")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var owner int
	var old *float64
	err = tx.QueryRowContext(r.Context(),
		`SELECT wholesaler_id, list_price FROM inventories WHERE id=$1 FOR UPDATE`, id).Scan(&owner, &old)
	if err == sql.ErrNoRows {
		fail(w, 404, "库存不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if owner != u.ID {
		fail(w, 403, "只能修改自己的库存价格")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO price_changes (inventory_id, old_price, new_price, reason, changed_by) VALUES ($1,$2,$3,$4,$5)`,
		id, old, in.NewPrice, in.Reason, u.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE inventories SET list_price=$1 WHERE id=$2`, in.NewPrice, id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:market")
	writeJSON(w, 200, map[string]any{"id": id, "old_price": old, "new_price": in.NewPrice})
}

// handleListPriceChanges 调价留痕（批发商月底对账）
func (a *app) handleListPriceChanges(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT p.id, p.old_price, p.new_price, p.reason, p.created_at,
	             b.batch_no, b.category, u.name, i.wholesaler_id
	      FROM price_changes p
	      JOIN inventories i ON i.id=p.inventory_id
	      JOIN flower_batches b ON b.id=i.batch_id
	      JOIN users u ON u.id=p.changed_by`
	args := []any{}
	if u.Role == "wholesaler" {
		q += " WHERE i.wholesaler_id=$1"
		args = append(args, u.ID)
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY p.id DESC LIMIT 200", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id, wid                  int
			oldP, newP               *float64
			reason, batchNo, cat, by string
			at                       time.Time
		)
		if err := rows.Scan(&id, &oldP, &newP, &reason, &at, &batchNo, &cat, &by, &wid); err != nil {
			continue
		}
		out = append(out, map[string]any{"id": id, "old_price": oldP, "new_price": newP, "reason": reason,
			"created_at": at, "batch_no": batchNo, "category": cat, "changed_by": by})
	}
	writeJSON(w, 200, out)
}

// ---------- 跨城调货 ----------

func (a *app) handleCreateTransfer(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	var in struct {
		InventoryID int    `json:"inventory_id"`
		Bundles     int    `json:"bundles"`
		ToCity      string `json:"to_city"`
	}
	if err := decodeBody(r, &in); err != nil || in.InventoryID == 0 || in.Bundles <= 0 || in.ToCity == "" {
		fail(w, 400, "请给出库存、扎数与目的城市")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var owner, avail int
	err = tx.QueryRowContext(r.Context(),
		`SELECT wholesaler_id, bundles_available FROM inventories WHERE id=$1 FOR UPDATE`, in.InventoryID).
		Scan(&owner, &avail)
	if err == sql.ErrNoRows {
		fail(w, 404, "库存不存在")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if owner != u.ID {
		fail(w, 403, "只能调拨自己的库存")
		return
	}
	if avail < in.Bundles {
		fail(w, 409, fmt.Sprintf("可调用扎数不足：剩余 %d 扎", avail))
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE inventories SET bundles_available=bundles_available-$1 WHERE id=$2`, in.Bundles, in.InventoryID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	var tid int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO transfers (inventory_id, wholesaler_id, bundles, to_city, created_by) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		in.InventoryID, u.ID, in.Bundles, in.ToCity, u.ID).Scan(&tid)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO deliveries (transfer_id, to_city, status) VALUES ($1,$2,'pending')`, tid, in.ToCity); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.rdb.Del(r.Context(), "cache:market")
	writeJSON(w, 201, map[string]any{"id": tid, "status": "pending", "to_city": in.ToCity})
}

func (a *app) handleListTransfers(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	q := `SELECT t.id, t.bundles, t.to_city, t.status, t.created_at, b.batch_no, b.category, u.name
	      FROM transfers t JOIN inventories i ON i.id=t.inventory_id
	      JOIN flower_batches b ON b.id=i.batch_id JOIN users u ON u.id=t.wholesaler_id`
	args := []any{}
	if u.Role == "wholesaler" {
		q += " WHERE t.wholesaler_id=$1"
		args = append(args, u.ID)
	}
	rows, err := a.db.QueryContext(r.Context(), q+" ORDER BY t.id DESC LIMIT 100", args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, bundles int
		var city, status, batchNo, cat, wname string
		var at time.Time
		if rows.Scan(&id, &bundles, &city, &status, &at, &batchNo, &cat, &wname) == nil {
			out = append(out, map[string]any{"id": id, "bundles": bundles, "to_city": city, "status": status,
				"created_at": at, "batch_no": batchNo, "category": cat, "wholesaler": wname})
		}
	}
	writeJSON(w, 200, out)
}
