package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// seedDemo 写入演示数据（仅当 users 表为空时执行，幂等）
func (a *app) seedDemo() {
	var n int
	if err := a.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil || n > 0 {
		return
	}
	log.Println("写入演示数据…")
	ctx := context.Background()
	now := time.Now()

	tx, err := a.db.Begin()
	if err != nil {
		log.Printf("演示数据写入失败: %v", err)
		return
	}
	defer tx.Rollback()

	hash, _ := bcrypt.GenerateFromPassword([]byte("flower123"), 10)
	uid := map[string]int{}
	addUser := func(username, role, name string, creditLimit float64, creditDays int) {
		var id int
		if err := tx.QueryRow(
			`INSERT INTO users (username, password_hash, role, name, credit_limit, credit_days)
			 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			username, string(hash), role, name, creditLimit, creditDays).Scan(&id); err != nil {
			log.Printf("seed 用户失败 %s: %v", username, err)
		}
		uid[username] = id
	}
	addUser("farmer01", "farmer", "花农·昆明老张", 0, 0)
	addUser("farmer02", "farmer", "花农·凌源小李", 0, 0)
	addUser("broker01", "broker", "经纪人·王姐", 0, 0)
	addUser("qc01", "qc", "质检员·陈工", 0, 0)
	addUser("wholesaler01", "wholesaler", "批发商·恒信花卉", 0, 0)
	addUser("wholesaler02", "wholesaler", "批发商·馨源贸易", 0, 0)
	addUser("shop01", "shop", "花店·城南花坊", 0, 0)
	addUser("shop02", "shop", "婚庆·喜结良缘(账期)", 50000, 30)
	addUser("shop03", "shop", "花店·直播间小艾", 0, 0)
	addUser("cold01", "coldstore", "冷库·赵班长", 0, 0)
	addUser("ship01", "logistics", "配送·孙师傅", 0, 0)
	addUser("fin01", "finance", "财务·刘会计", 0, 0)
	addUser("boss01", "manager", "市场管理·吴经理", 0, 0)

	slotID := map[string]int{}
	addSlot := func(code, zone string, cap, occ int) {
		var id int
		if err := tx.QueryRow(
			`INSERT INTO storage_slots (code, zone, capacity_bundles, occupied_bundles) VALUES ($1,$2,$3,$4) RETURNING id`,
			code, zone, cap, occ).Scan(&id); err != nil {
			log.Printf("seed 库位失败: %v", err)
		}
		slotID[code] = id
	}
	addSlot("A-01", "A", 300, 80)
	addSlot("A-02", "A", 300, 60)
	addSlot("A-03", "A", 300, 85)
	addSlot("A-04", "A", 300, 0)
	addSlot("B-01", "B", 150, 40)
	addSlot("B-02", "B", 150, 0)

	var pres1, pres2 int
	tx.QueryRow(`INSERT INTO preservative_batches (code, brand, ingredient, expires_at)
		VALUES ('PB-2601','可利鲜','蔗糖+杀菌剂（玫瑰专用）',$1) RETURNING id`, now.AddDate(0, 6, 0)).Scan(&pres1)
	tx.QueryRow(`INSERT INTO preservative_batches (code, brand, ingredient, expires_at)
		VALUES ('PB-2602','花之寿','有机酸+乙烯抑制剂（百合/郁金香）',$1) RETURNING id`, now.AddDate(0, 4, 0)).Scan(&pres2)

	addBatch := func(farmer string, cat, origin string, cutAt time.Time, stem, open float64,
		bundles, spb int, temp float64, sellH int, grade, flags, status string,
		slotCode string, storedAt *time.Time) int {
		var id int
		var slot *int
		if slotCode != "" {
			if sid, ok := slotID[slotCode]; ok {
				slot = &sid
			}
		}
		if err := tx.QueryRow(
			`INSERT INTO flower_batches (farmer_id, category, origin, cut_at, stem_length_cm, openness,
			  bundles, stems_per_bundle, arrival_temp_c, sellable_hours, grade, risk_flags, status,
			  storage_slot_id, stored_at, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
			uid[farmer], cat, origin, cutAt, stem, open, bundles, spb, temp, sellH,
			grade, flags, status, slot, storedAt, cutAt.Add(2*time.Hour)).Scan(&id); err != nil {
			log.Printf("seed 批次失败: %v", err)
			return 0
		}
		tx.Exec(`UPDATE flower_batches SET batch_no=$1 WHERE id=$2`, fmt.Sprintf("B%06d", id), id)
		return id
	}
	addQC := func(batchID int, grade string, even, short float64, tempOK bool, notes string, at time.Time) {
		tx.Exec(`INSERT INTO qc_records (batch_id, inspector_id, grade, openness_evenness, short_stem_ratio, temp_ok, notes, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, batchID, uid["qc01"], grade, even, short, tempOK, notes, at)
	}
	h := time.Hour
	m := time.Minute

	// 批次1：玫瑰·昆明 —— 质检A，已入库，正在竞价（35 分钟后结拍）
	t1 := now.Add(-6 * h)
	b1 := addBatch("farmer01", "rose", "云南昆明", now.Add(-10*h), 70, 3.0, 80, 20, 4.5, 48, "A", "", "in_auction", "A-01", &t1)
	addQC(b1, "A", 0.92, 0.05, true, "花头整齐，枝杆挺拔", now.Add(-5*h))
	var auc1 int
	tx.QueryRow(`INSERT INTO auctions (batch_id, mode, start_price, min_increment, current_price, status, starts_at, ends_at, created_by)
		VALUES ($1,'auction',38,1,41,'active',$2,$3,$4) RETURNING id`,
		b1, now.Add(-25*m), now.Add(35*m), uid["boss01"]).Scan(&auc1)
	tx.Exec(`INSERT INTO bids (auction_id, wholesaler_id, price, created_at) VALUES ($1,$2,39,$3)`, auc1, uid["wholesaler01"], now.Add(-20*m))
	tx.Exec(`INSERT INTO bids (auction_id, wholesaler_id, price, created_at) VALUES ($1,$2,41,$3)`, auc1, uid["wholesaler02"], now.Add(-8*m))

	// 批次2：百合·凌源 —— 质检B，已入库待上拍
	t2 := now.Add(-5 * h)
	b2 := addBatch("farmer02", "lily", "辽宁凌源", now.Add(-9*h), 65, 2.5, 60, 10, 5.0, 60, "B", "", "stored", "A-02", &t2)
	addQC(b2, "B", 0.85, 0.08, true, "个别花苞偏生", now.Add(-4*h))

	// 批次3：郁金香·昆明 —— 刚到货待质检
	addBatch("farmer01", "tulip", "云南昆明", now.Add(-3*h), 45, 2.0, 50, 10, 6.0, 36, "", "", "qc_pending", "", nil)

	// 批次4：洋桔梗·广州 —— 质检D 打回（开放度不均+短枝比例高）
	b4 := addBatch("farmer02", "eustoma", "广东广州", now.Add(-8*h), 40, 3.5, 40, 10, 9.5, 30, "D",
		"uneven_openness,high_short_stem,temp_abnormal", "qc_flagged", "", nil)
	addQC(b4, "D", 0.55, 0.32, false, "开放度不均，短枝比例 32%，冷链温度偏高", now.Add(-3*h))

	// 批次5：节日混搭·昆明 —— 批发商01 拍得 100 扎@40，部分已售/退货/调货
	t5 := now.Add(-30 * h)
	b5 := addBatch("farmer01", "holiday_mix", "云南昆明", now.Add(-34*h), 60, 3.0, 100, 15, 5.5, 72, "A", "", "sold", "A-03", &t5)
	addQC(b5, "A", 0.9, 0.06, true, "混搭比例均匀", now.Add(-29*h))
	var auc5 int
	tx.QueryRow(`INSERT INTO auctions (batch_id, mode, start_price, min_increment, current_price, winner_id, status, starts_at, ends_at, created_by)
		VALUES ($1,'auction',35,1,40,$2,'closed',$3,$4,$5) RETURNING id`,
		b5, uid["wholesaler01"], now.Add(-29*h), now.Add(-28*h), uid["boss01"]).Scan(&auc5)
	tx.Exec(`INSERT INTO bids (auction_id, wholesaler_id, price, created_at) VALUES ($1,$2,40,$3)`, auc5, uid["wholesaler01"], now.Add(-28*h))
	var inv5 int
	tx.QueryRow(`INSERT INTO inventories (wholesaler_id, batch_id, auction_id, bundles_total, bundles_available,
		bundles_reserved, cost_price, list_price, listed, created_at)
		VALUES ($1,$2,$3,100,60,5,40,52,true,$4) RETURNING id`,
		uid["wholesaler01"], b5, auc5, now.Add(-28*h)).Scan(&inv5)
	tx.Exec(`INSERT INTO price_changes (inventory_id, old_price, new_price, reason, changed_by, created_at)
		VALUES ($1,48,52,'节前需求上涨，临时调价',$2,$3)`, inv5, uid["wholesaler01"], now.Add(-12*h))

	addOrder := func(shop string, invID, bundles int, price float64, purpose, payType, status string,
		presale bool, festival string, deadline time.Time, paidAt, deliveredAt *time.Time, createdAt time.Time) int {
		var id int
		if err := tx.QueryRow(
			`INSERT INTO orders (shop_id, inventory_id, wholesaler_id, batch_id, bundles, price_per_bundle,
			  total_amount, purpose, payment_type, status, is_presale, festival, payment_deadline, paid_at, delivered_at, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
			uid[shop], invID, uid["wholesaler01"], b5, bundles, price, float64(bundles)*price,
			purpose, payType, status, presale, festival, deadline, paidAt, deliveredAt, createdAt).Scan(&id); err != nil {
			log.Printf("seed 订单失败: %v", err)
			return 0
		}
		tx.Exec(`UPDATE orders SET order_no=$1 WHERE id=$2`, fmt.Sprintf("O%08d", id), id)
		return id
	}
	// 订单1：shop01 零售 10 扎，已送达 → 掉瓣退货（已裁定）
	p1 := now.Add(-27 * h)
	d1 := now.Add(-20 * h)
	o1 := addOrder("shop01", inv5, 10, 52, "retail", "immediate", "delivered", false, "",
		now.Add(-26*h), &p1, &d1, now.Add(-27*h))
	tx.Exec(`INSERT INTO payments (order_id, amount, method, recorded_by, paid_at) VALUES ($1,520,'online',$2,$3)`,
		o1, uid["shop01"], p1)
	tx.Exec(`INSERT INTO deliveries (order_id, dispatcher_id, to_city, status, departed_at, delivered_at, created_at)
		VALUES ($1,$2,'本市','delivered',$3,$4,$5)`, o1, uid["ship01"], now.Add(-26*h), d1, now.Add(-26*h))
	var ret1 int
	tx.QueryRow(`INSERT INTO returns (order_id, batch_id, shop_id, bundles, reason, loss_ratio, claim_amount,
		status, reviewed_by, reviewed_at, farmer_responsible, review_note, created_at)
		VALUES ($1,$2,$3,2,'petal_drop',0.3,31.2,'approved',$4,$5,true,'到店掉瓣属实，采切成熟度偏高',$6) RETURNING id`,
		o1, b5, uid["shop01"], uid["qc01"], now.Add(-18*h), now.Add(-19*h)).Scan(&ret1)
	tx.Exec(`INSERT INTO farmer_credit_events (farmer_id, delta, reason, return_id, created_at)
		VALUES ($1,-3,'退货判定花农责任（到店掉瓣）',$2,$3)`, uid["farmer01"], ret1, now.Add(-18*h))
	tx.Exec(`UPDATE users SET credit_score=97 WHERE id=$1`, uid["farmer01"])

	// 订单2：shop02 婚庆账期 20 扎，已逾期 1 小时
	p2 := now.Add(-26 * h)
	o2 := addOrder("shop02", inv5, 20, 52, "wedding", "credit", "paid", false, "",
		now.Add(-25*h), &p2, nil, now.Add(-26*h))
	tx.Exec(`INSERT INTO payments (order_id, amount, method, recorded_by, paid_at) VALUES ($1,1040,'credit',$2,$3)`,
		o2, uid["shop02"], p2)
	tx.Exec(`INSERT INTO credit_receivables (order_id, customer_id, amount, due_date, status, created_at)
		VALUES ($1,$2,1040,$3,'overdue',$4)`, o2, uid["shop02"], now.Add(-1*h), p2)

	// 订单3：shop03 直播花束 5 扎，待付款（25 分钟后超时）
	o3 := addOrder("shop03", inv5, 5, 52, "livestream", "immediate", "pending_payment", false, "",
		now.Add(25*m), nil, nil, now.Add(-5*m))
	_ = o3

	// 批次6：尤加利·昆明 —— 批发商02 拍得并上架，含一笔节日预售锁货
	t6 := now.Add(-9 * h)
	b6 := addBatch("farmer02", "eucalyptus", "云南昆明", now.Add(-13*h), 55, 2.0, 40, 10, 4.0, 96, "B", "", "sold", "B-01", &t6)
	addQC(b6, "B", 0.88, 0.1, true, "叶色正常", now.Add(-8*h))
	var auc6 int
	tx.QueryRow(`INSERT INTO auctions (batch_id, mode, start_price, buy_now_price, min_increment, current_price, winner_id, status, starts_at, ends_at, created_by)
		VALUES ($1,'fixed',15,15,1,15,$2,'closed',$3,$4,$5) RETURNING id`,
		b6, uid["wholesaler02"], now.Add(-9*h), now.Add(-8*h), uid["boss01"]).Scan(&auc6)
	tx.Exec(`INSERT INTO bids (auction_id, wholesaler_id, price, created_at) VALUES ($1,$2,15,$3)`, auc6, uid["wholesaler02"], now.Add(-8*h))
	var inv6 int
	tx.QueryRow(`INSERT INTO inventories (wholesaler_id, batch_id, auction_id, bundles_total, bundles_available,
		bundles_reserved, cost_price, list_price, listed, created_at)
		VALUES ($1,$2,$3,40,35,5,15,22,true,$4) RETURNING id`,
		uid["wholesaler02"], b6, auc6, now.Add(-8*h)).Scan(&inv6)
	// 节日预售锁货：shop02 为七夕预售 5 扎尤加利
	addOrderWithWholesaler := func(shop string, invID, wholesalerID, batchID, bundles int, price float64,
		purpose, payType, status string, presale bool, festival string, deadline time.Time, createdAt time.Time) {
		var id int
		if err := tx.QueryRow(
			`INSERT INTO orders (shop_id, inventory_id, wholesaler_id, batch_id, bundles, price_per_bundle,
			  total_amount, purpose, payment_type, status, is_presale, festival, payment_deadline, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
			uid[shop], invID, wholesalerID, batchID, bundles, price, float64(bundles)*price,
			purpose, payType, status, presale, festival, deadline, createdAt).Scan(&id); err != nil {
			log.Printf("seed 预售单失败: %v", err)
			return
		}
		tx.Exec(`UPDATE orders SET order_no=$1 WHERE id=$2`, fmt.Sprintf("O%08d", id), id)
	}
	addOrderWithWholesaler("shop02", inv6, uid["wholesaler02"], b6, 5, 22, "wedding", "immediate",
		"pending_payment", true, "七夕", now.Add(72*h), now.Add(-2*h))

	// 跨城调货：批发商01 调 5 扎节日混搭到上海，在途
	var tr1 int
	tx.QueryRow(`INSERT INTO transfers (inventory_id, wholesaler_id, bundles, to_city, status, created_by, created_at)
		VALUES ($1,$2,5,'上海','in_transit',$3,$4) RETURNING id`,
		inv5, uid["wholesaler01"], uid["wholesaler01"], now.Add(-2*h)).Scan(&tr1)
	tx.Exec(`INSERT INTO deliveries (transfer_id, dispatcher_id, to_city, status, departed_at, created_at)
		VALUES ($1,$2,'上海','in_transit',$3,$4)`, tr1, uid["ship01"], now.Add(-2*h), now.Add(-2*h))

	// 保鲜剂施用：批次1 玫瑰用可利鲜
	tx.Exec(`INSERT INTO batch_preservatives (batch_id, preservative_id, dosage_ml, applied_by, applied_at)
		VALUES ($1,$2,500,$3,$4)`, b1, pres1, uid["cold01"], now.Add(-5*h))
	tx.Exec(`UPDATE flower_batches SET sellable_hours=sellable_hours+12 WHERE id=$1`, b1)
	_ = pres2

	// ===== 冷库库位不足调度演示 =====
	// 花材敏感度配置（越大越怕开放、越优先入库）
	for cat, sens := range map[string]int{
		"rose": 5, "eustoma": 4, "tulip": 4, "lily": 3, "holiday_mix": 3, "eucalyptus": 1,
	} {
		tw := map[int]string{5: "2-4℃ 优先", 4: "2-5℃", 3: "3-6℃", 1: "4-8℃ 耐放"}[sens]
		tx.Exec(`INSERT INTO category_sensitivity (category, sensitivity, temp_window) VALUES ($1,$2,$3)
			ON CONFLICT (category) DO NOTHING`, cat, sens, tw)
	}

	// 用凌晨到货的在库批次把各库位补到接近满载（每库位仅余 10-15 扎），制造爆满
	fillSlot := func(slotCode string, fillBundles int, cat string) {
		st := now.Add(-3 * h)
		fid := addBatch("farmer02", cat, "云南玉溪", now.Add(-7*h), 60, 2.5, fillBundles, 20, 5.0, 48,
			"B", "", "stored", slotCode, &st)
		addQC(fid, "B", 0.85, 0.08, true, "凌晨高峰入库", now.Add(-3*h))
	}
	fillSlot("A-01", 205, "lily")
	fillSlot("A-02", 225, "rose")
	fillSlot("A-03", 200, "holiday_mix")
	fillSlot("A-04", 285, "eustoma")
	fillSlot("B-01", 100, "eucalyptus")
	fillSlot("B-02", 140, "lily")
	// 按实际在库批次重算库位占用（补满至每库位仅余 10-15 扎，制造凌晨爆满）
	tx.Exec(`UPDATE storage_slots s SET occupied_bundles = COALESCE(
		(SELECT sum(bundles) FROM flower_batches WHERE storage_slot_id=s.id AND out_at IS NULL), 0)`)

	// 已成交但待入库的批次需要库存行
	soldInventory := func(batchID int, wholesaler string, bundles int, price float64) {
		tx.Exec(`INSERT INTO inventories (wholesaler_id, batch_id, bundles_total, bundles_available, cost_price, listed, status, created_at)
			VALUES ($1,$2,$3,$3,$4,false,'in_stock',$5)`, uid[wholesaler], batchID, bundles, price, now.Add(-1*h))
	}

	// 等待入库批次（均 30 扎以上，爆满期间任何库位都放不下）
	eta7 := now.Add(50 * m)  // 玫瑰：高敏感 + 早提货
	eta8 := now.Add(3 * h)   // 尤加利：低敏感 + 晚提货
	eta9 := now.Add(40 * m)  // 百合：已成交
	eta10 := now.Add(20 * m) // 郁金香：已成交，临时保温失败（见下）
	b7 := addBatch("farmer01", "rose", "云南昆明", now.Add(-2*h), 68, 2.0, 40, 20, 6.5, 48,
		"A", "", "qc_passed", "", nil)
	tx.Exec(`UPDATE flower_batches SET broker_id=$1 WHERE id=$2`, uid["broker01"], b7)
	b8 := addBatch("farmer02", "eucalyptus", "云南昆明", now.Add(-2*h), 55, 1.5, 35, 10, 6.0, 96,
		"B", "", "qc_passed", "", nil)
	b9 := addBatch("farmer02", "lily", "辽宁凌源", now.Add(-3*h), 62, 2.5, 45, 10, 6.0, 60,
		"B", "", "sold", "", nil)
	tx.Exec(`UPDATE flower_batches SET broker_id=$1 WHERE id=$2`, uid["broker01"], b9)
	soldInventory(b9, "wholesaler01", 45, 24)
	b10 := addBatch("farmer01", "tulip", "云南昆明", now.Add(-4*h), 45, 2.0, 30, 10, 7.0, 36,
		"B", "", "sold", "", nil)
	tx.Exec(`UPDATE flower_batches SET broker_id=$1 WHERE id=$2`, uid["broker01"], b10)
	soldInventory(b10, "wholesaler02", 30, 18)
	addQC(b7, "A", 0.9, 0.05, true, "", now.Add(-90*m))
	addQC(b8, "B", 0.86, 0.08, true, "", now.Add(-80*m))
	addQC(b9, "B", 0.83, 0.09, true, "", now.Add(-2*h))
	addQC(b10, "B", 0.82, 0.1, true, "", now.Add(-3*h))

	addQueue := func(batchID int, farmer, broker, wholesaler string, sens int, isSold bool,
		eta time.Time, method, resp, qstatus string) int {
		var qid int
		var brokerArg, wholeArg any
		if broker != "" {
			brokerArg = uid[broker]
		}
		if wholesaler != "" {
			wholeArg = uid[wholesaler]
		}
		tx.QueryRow(`INSERT INTO cold_storage_queue
			(batch_id, farmer_id, broker_id, wholesaler_id, sensitivity, is_sold, pickup_eta, temp_method,
			 temp_method_note, loss_responsibility, loss_note, priority_score, status, queued_at, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
			batchID, uid[farmer], brokerArg, wholeArg,
			sens, isSold, eta, method, "凌晨爆满，库外临时保温", resp,
			"出险后按预案并由冷库/批发商双方确认",
			coldPriorityScore(sens, isSold, &eta), qstatus, now.Add(-30*m), uid["cold01"]).Scan(&qid)
		return qid
	}
	_ = addQueue(b7, "farmer01", "broker01", "", 5, false, eta7, "ice_pack", "farmer", "waiting")
	_ = addQueue(b8, "farmer02", "", "", 1, false, eta8, "ventilated", "shared", "waiting")
	_ = addQueue(b9, "farmer02", "broker01", "wholesaler01", 3, true, eta9, "quilt", "wholesaler", "waiting")
	q10 := addQueue(b10, "farmer01", "broker01", "wholesaler02", 4, true, eta10, "ice_pack", "shared", "failed")

	// 事故1：郁金香临时保温失败开放过快 —— 冷库与批发商均已确认责任，冷库已复盘，
	// 市场已发起该时段入场顺序调整并通知经纪人，经纪人待确认
	var inc1 int
	tx.QueryRow(`INSERT INTO cold_chain_incidents
		(queue_id, batch_id, reported_by, opened_at, openness_before, openness_after, description, temp_method,
		 coldstore_responsible, wholesaler_responsible, coldstore_confirmed_by, coldstore_confirmed_at,
		 wholesaler_confirmed_by, wholesaler_confirmed_at, status, reviewed_by, reviewed_at, review_note, closed_at)
		VALUES ($1,$2,$3,$4,2.0,4.5,$5,'ice_pack',true,true,$6,$7,$8,$9,'reviewed',$6,$10,$11,$10) RETURNING id`,
		q10, b10, uid["cold01"], now.Add(-2*h),
		"加冰瓶保温失败，郁金香集中开放，无法赶上早场提货",
		uid["cold01"], now.Add(-100*m), uid["wholesaler02"], now.Add(-95*m),
		now.Add(-70*m), "冷库临时保温位不足 + 批发商提货延迟，双方共责；已增配临时冷藏车").Scan(&inc1)
	var adj1 int
	tx.QueryRow(`INSERT INTO entry_order_adjustments
		(incident_id, time_window, wholesaler_id, broker_id, adjustment, effective_date, status, created_by, notified_at)
		VALUES ($1,'03:00-05:00',$2,$3,$4,$5,'notified',$6,$7) RETURNING id`,
		inc1, uid["wholesaler02"], uid["broker01"],
		"03:00-05:00 高峰时段：已成交且敏感度高的花材优先进场，经纪人代录批次提前 30 分钟到场，临时保温车就位",
		now, uid["boss01"], now.Add(-60*m)).Scan(&adj1)
	tx.Exec(`UPDATE cold_chain_incidents SET adjustment_id=$1 WHERE id=$2`, adj1, inc1)
	tx.Exec(`INSERT INTO notifications (user_id, type, title, body, ref_type, ref_id, acked, is_read, created_at)
		VALUES ($1,'entry_adjustment',$2,$3,'adjustment',$4,true,false,$5)`,
		uid["broker01"], "凌晨入场顺序调整通知（03:00-05:00 时段）",
		"依据郁金香保温失败复盘：已成交高敏感花材优先进场，代录批次提前 30 分钟到场，请确认知悉。",
		adj1, now.Add(-60*m))

	// 事故2：百合临时保温疑似失败开放过快 —— 刚上报，冷库负责人与批发商均待确认责任
	// （独立暂存区发生的事故，未关联等待队列：该批次仍在另一保温位正常排队）
	var inc2 int
	tx.QueryRow(`INSERT INTO cold_chain_incidents
		(queue_id, batch_id, reported_by, opened_at, openness_before, openness_after, description, temp_method, status)
		VALUES (NULL,$1,$2,$3,2.5,4.0,$4,'quilt','pending') RETURNING id`,
		b9, uid["cold01"], now.Add(-15*m),
		"保温棉被覆盖后温度回升，百合开放加快，等待双方确认责任").Scan(&inc2)
	tx.Exec(`INSERT INTO notifications (user_id, type, title, body, ref_type, ref_id, acked, is_read, created_at)
		VALUES ($1,'incident',$2,$3,'incident',$4,false,false,$5)`,
		uid["cold01"], "保温失败待确认责任：百合批次开放过快", "开放度 2.5 → 4.0，请冷库负责人确认责任。", inc2, now.Add(-15*m))
	tx.Exec(`INSERT INTO notifications (user_id, type, title, body, ref_type, ref_id, acked, is_read, created_at)
		VALUES ($1,'incident',$2,$3,'incident',$4,false,false,$5)`,
		uid["wholesaler01"], "保温失败待确认责任：百合批次开放过快", "开放度 2.5 → 4.0，请批发商确认责任。", inc2, now.Add(-15*m))

	if err := tx.Commit(); err != nil {
		log.Printf("演示数据提交失败: %v", err)
		return
	}
	a.syncFarmerRanking(ctx)
	log.Println("演示数据就绪：13 个账号、6 个批次、1 场进行中的竞价、库存/订单/退货/账期/调货样例")
}
