# 鲜花批发市场 · 凌晨竞价分货与损耗服务

凌晨鲜花批发市场的一站式服务：花农/经纪人凌晨到货登记 → 质检定级 → 冷库入库 → 管理方发起竞价/一口价 → 批发商拍货上架 → 花店按婚礼/零售/直播/企业订单锁货采购 → 配送到店 → 退货裁定与损耗 → 财务按成交价、损耗比例、冷库时长、退货原因、账期扣款生成月度结算。市场管理方通过看板掌握各品类/产地的成交速度、冷库占用、退货损耗与账期风险，退货判定会扣减花农信用并影响下次竞价排序。

技术栈：**Go 1.26（标准库 net/http 路由）+ PostgreSQL 16 + Redis 7**，单页前端内嵌于 Go 二进制。

## 原始需求

实现鲜花批发市场凌晨竞价分货与损耗服务，可使用 Go、PostgreSQL 和 Redis。花农或经纪人凌晨送达玫瑰、百合、郁金香、洋桔梗、尤加利和节日混搭花材时，服务记录产地、采切时间、枝长、开放度、扎数、冷链到货温度、质检等级和可售时长。批发商按等级发起竞价或一口价，花店采购根据婚礼、门店零售、直播花束和企业订单锁定货品。若同批花材开放度不均、短枝比例过高、冷库库位不足、花店超时未付款、批发商临时改价、节日前订单被拆分、花材到店后掉瓣或账期客户逾期，服务要把花农、质检、批发商、花店采购、冷库、配送和财务连接到同一批花材。最终结算需要按成交价、损耗比例、冷库时长、退货原因和账期扣款生成，帮助市场判断哪些产地和品类更稳定。服务还要支持节日预售锁货、婚庆公司账期、直播花束备货、跨城调货和花材保鲜剂批次。市场管理方需要按品类和产地看到成交速度、冷库占用、退货损耗和账期风险，便于调整凌晨拍卖秩序。退货判定会影响花农信用和下次竞价排序。市场还要保留每次调价和退货对账，方便批发商月底结算。

## 架构与组成

| 服务 | 镜像 | 说明 |
|---|---|---|
| app | 本地多阶段构建（golang:1.26-alpine → alpine:3.21，非 root 运行，含 HEALTHCHECK） | REST API + 内嵌单页前端，监听 8080 |
| db | postgres:16-alpine | 业务库，仅 compose 内部网络，不发布宿主端口 |
| redis | redis:7-alpine | 会话、锁、缓存、排名，仅 compose 内部网络 |

Redis 的用途：登录会话（`sess:*`，8h）、下单防超卖互斥锁（`lock:inv:*`）、采购市场列表缓存（`cache:market` 5s）、看板缓存（`cache:overview` 10s）、竞价实时最高价（`auction:top:*`）、花农信用有序集合（`farmer:credit`，决定竞价大厅排序）。

后台清扫器（默认 15s 一轮）：到期拍卖自动结拍 → 生成批发商库存；超时未付款订单自动取消并释放锁货；账期应收自动标记逾期；超过可售时长未售出的批次自动报损并释放冷库库位。

## 冷库库位不足调度（凌晨到货高峰）

凌晨到货高峰冷库爆满、`POST /api/batches/{id}/store` 因余量不足返回 409 时，进入调度：

1. **登记排队与临时保温**：冷库（或花农/经纪人）为质检通过但无法入库的批次登记 `cold_storage_queue`：**临时保温方式**（加冰瓶/保温棉被/通风暂存区/遮阳/风机/其他）与**损耗责任预案**（花农/冷库/批发商/共同承担），并填写预计提货时间。
2. **优先入库顺序**：`花材敏感度（高→低）→ 已成交（已成交优先）→ 预计提货时间（早→晚）→ 先到先排`。敏感度按品类配置（`category_sensitivity`，市场可调；缺省 玫瑰5 > 洋桔梗/郁金香4 > 百合/混搭3 > 尤加利1）。冷库负责人在「冷库调度」页按此顺序放行，放行仍逐库位校验余量，腾出库位后才能入库。
3. **临时保温失败（花材开放过快）**：冷库/质检上报事故（保温前后开放度、方式、说明），系统通知冷库负责人与该批次拍得批发商。
4. **双方确认责任**：冷库负责人与批发商**分别**确认各自责任（`coldstore_confirmed_*` / `wholesaler_confirmed_*`，非拍得批发商无权确认，市场可代行）；双方齐备前不得复盘。
5. **冷库端同步复盘**：双方确认后冷库负责人提交复盘结论与改进措施，事故结案。
6. **调整该时段入场顺序**：市场管理方依据复盘为指定时段（如 03:00-05:00）发起入场顺序调整，自动**通知经纪人**（批次有经纪人则定向通知，没有则通知全体经纪人）。
7. **经纪人确认**：经纪人在通知/调度页确认知悉或反馈异议，结果回传市场。事故、责任、复盘、调整、确认全过程留痕，并在市场看板汇总计数。

## 快速开始（验证方式 = 宿主 docker compose up）

```bash
cp .env.example .env        # 可选，默认值即可运行
docker compose up -d --build
docker compose ps           # app 健康后（healthy）即可访问
docker compose port app 8080  # 查看实际映射端口
# 浏览器打开 http://localhost:${CC_PUBLISH_PORT} （或宿主机映射端口）
```

- 应用是唯一发布到宿主的端口：`${CC_PUBLISH_PORT}:8080`；db/redis 仅在 compose 内部网络。
- 首次启动自动建表并写入演示数据（`SEED_DEMO=false` 可关闭）。
- 验证结束：`docker compose down`（加 `-v` 可清空数据卷）。

## 测试账号（密码统一 `flower123`）

| 用户名 | 角色 | 权限要点 |
|---|---|---|
| farmer01 / farmer02 | 花农 | 凌晨到货登记、查看自己批次与结算 |
| broker01 | 经纪人 | 代花农登记批次（需指定花农）、接收入场顺序调整通知并确认 |
| qc01 | 质检 | 质检定级（开放度均匀度/短枝比例/冷链）、退货裁定（可判花农责任扣信用）、上报保温失败 |
| wholesaler01 / wholesaler02 | 批发商 | 竞价/一口价拍货、上架、临时改价（留痕）、跨城调货、查看退货对账与结算、**确认保温事故批发商侧责任** |
| shop01 | 花店采购（零售） | 采购市场锁货下单、支付、拆分、退货 |
| shop02 | 婚庆公司（**账期客户**，额度 5 万 / 30 天） | 可账期挂账采购、节日预售锁货 |
| shop03 | 花店（直播） | 直播花束备货采购 |
| cold01 | 冷库 | 库位管理、批次入库（库位不足会拒绝）、保鲜剂批次与施用、**库位不足调度（登记临时保温/按优先级放行/双方责任确认/冷库端复盘）** |
| ship01 | 配送 | 订单发货、跨城调货运输、确认送达 |
| fin01 | 财务 | 账期应收核销、月度结算生成、调价记录、退货对账 |
| boss01 | 市场管理方 | 发起竞价/一口价专场、提前结拍、市场看板、花农信用排名、**维护花材敏感度、依据复盘调整凌晨入场顺序** |

## 演示数据（首次启动写入）

- 批次：玫瑰（质检A·在库·**竞价中**，35 分钟后自动结拍）、百合（B·在库待拍）、郁金香（刚到货待质检）、洋桔梗（D 级打回：开放度不均+短枝高+失温）、节日混搭（批发商01 已拍得并上架，含已送达订单的**掉瓣退货已赔付**记录、婚庆**账期逾期**应收、直播待付款订单、**跨城调货在途**）、尤加利（批发商02 一口价拍得，含**七夕预售锁货**单）。
- 冷库：A-01~A-04（300 扎）、B-01/B-02（150 扎），**凌晨高峰被补至接近爆满（每库位仅余 0-15 扎）**。
- **冷库调度样例**：等待入库队列 3 批（高敏感玫瑰未成交 / 已成交百合 / 低敏感尤加利，按「敏感度→已成交→预计提货时间」排序，均登记临时保温方式与损耗责任预案）；1 起郁金香保温失败事故**已双方确认+冷库复盘+入场调整待经纪人确认**；1 起百合事故刚上报**待冷库与批发商确认**；另有 1 批已成交郁金香因保温失败退出排队。
- 花材敏感度（市场可调）：玫瑰 5 > 洋桔梗/郁金香 4 > 百合/节日混搭 3 > 尤加利 1。
- 保鲜剂：PB-2601 可利鲜（玫瑰）、PB-2602 花之寿。
- 花农信用：farmer01 因掉瓣退货被判责任，信用 97（初始 100），竞价大厅排序已靠后。

## 关键业务流验证（curl）

```bash
BASE=http://localhost:${CC_PUBLISH_PORT}
# 登录拿 token（各角色替换 username 即可）
TOK=$(curl -s $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"farmer01","password":"flower123"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
AUTH="Authorization: Bearer $TOK"

# 1) 花农登记批次 → 2) 质检 → 3) 冷库入库
curl -s $BASE/api/batches -X POST -H "$AUTH" -H 'Content-Type: application/json' -d \
 '{"category":"rose","origin":"云南玉溪","cut_at":"2026-09-25T02:30","stem_length_cm":65,"openness":3,"bundles":60,"stems_per_bundle":20,"arrival_temp_c":4.5,"sellable_hours":48}'
curl -s $BASE/api/batches/7/qc -X POST -H "$AUTH" ...   # 用 qc01 的 token：grade/evenness/short_stem_ratio/temp_ok
curl -s $BASE/api/batches/7/store -X POST ...           # 用 cold01 的 token：{"slot_id":4}

# 4) 管理方发起竞价 → 5) 批发商出价 → 6) 结拍生成库存
curl -s $BASE/api/auctions -X POST ...                  # boss01：{"batch_id":7,"mode":"auction","start_price":30,"duration_min":30}
curl -s $BASE/api/auctions/2/bid -X POST ...            # wholesaler01：{"price":35}
curl -s $BASE/api/auctions/2/close -X POST ...          # boss01（或等清扫器自动结拍）

# 7) 批发商上架/改价 → 8) 花店锁货下单 → 9) 支付（shop02 可 payment_type=credit 账期）
curl -s $BASE/api/inventories/3/listing -X POST ...     # {"listed":true,"list_price":45}
curl -s $BASE/api/orders -X POST ...                    # shop01：{"inventory_id":3,"bundles":5,"purpose":"retail","payment_type":"immediate"}
curl -s $BASE/api/orders/4/pay -X POST ...

# 10) 配送发车/送达 → 11) 花店退货（掉瓣）→ 12) 质检裁定（判花农责任 → 扣信用）
curl -s $BASE/api/deliveries -X POST ...                # ship01：{"order_id":4,"to_city":"本市"}
curl -s $BASE/api/deliveries/3/status -X POST ...       # {"status":"delivered"}
curl -s $BASE/api/orders/4/returns -X POST ...          # {"bundles":1,"reason":"petal_drop","loss_ratio":0.3}
curl -s $BASE/api/returns/2/review -X POST ...          # qc01：{"approve":true,"farmer_responsible":true}

# 13) 财务：应收/核销、月度结算、退货对账、调价记录
curl -s $BASE/api/receivables -H "$AUTH"
curl -s $BASE/api/settlements/generate -X POST ...      # {"month":"2026-09"}
curl -s "$BASE/api/returns/reconciliation?month=2026-09"
curl -s $BASE/api/price-changes

# 14) 管理方看板
curl -s $BASE/api/analytics/overview
curl -s $BASE/api/analytics/stability
curl -s $BASE/api/farmers/ranking

# 15) 冷库库位不足调度（凌晨高峰爆满）
curl -s $BASE/api/coldchain/board -H "$AUTH"                     # 冷链接看板：占用/等待队列/事故/入场调整
curl -s $BASE/api/coldchain/queue -H "$AUTH"                     # 调度队列（已按敏感度→已成交→提货时间排序）
# cold01：为无法入库的批次登记临时保温方式与损耗责任并入队
curl -s $BASE/api/coldchain/queue -X POST -H "$AUTH" -H 'Content-Type: application/json' -d \
 '{"batch_id":7,"pickup_eta":"2026-09-27T06:00","temp_method":"ice_pack","loss_responsibility":"shared","loss_note":"加冰瓶暂存"}'
# cold01：腾出空位后按调度顺序放行（仍校验余量，不足返回 409）
curl -s $BASE/api/coldchain/queue/1/admit -X POST -H "$AUTH" -H 'Content-Type: application/json' -d '{"slot_id":6}'
# cold01/qc01：上报临时保温失败（开放过快），自动通知冷库负责人与批发商
curl -s $BASE/api/coldchain/incidents -X POST -H "$AUTH" -H 'Content-Type: application/json' -d \
 '{"batch_id":10,"queue_id":4,"openness_before":2.0,"openness_after":4.5,"temp_method":"ice_pack","description":"集中开放"}'
# 冷库负责人与批发商分别确认责任（批发商仅该批次拍得方可确认；boss01 可代任一方）
curl -s $BASE/api/coldchain/incidents/2/confirm -X POST -H "$AUTH" -H 'Content-Type: application/json' -d '{"side":"coldstore","responsible":true,"note":"保温位不足"}'
curl -s $BASE/api/coldchain/incidents/2/confirm -X POST -H "$AUTH" -H 'Content-Type: application/json' -d '{"side":"wholesaler","responsible":true,"note":"提货延迟"}'
# 双方确认齐后：冷库端同步复盘
curl -s $BASE/api/coldchain/incidents/2/review -X POST -H "$AUTH" -H 'Content-Type: application/json' -d '{"note":"增配临时冷藏车，高峰加密巡查"}'
# boss01：依据复盘调整该时段入场顺序（自动通知该批次经纪人；无经纪人则通知全体经纪人）
curl -s $BASE/api/coldchain/incidents/2/adjustment -X POST -H "$AUTH" -H 'Content-Type: application/json' -d \
 '{"time_window":"03:00-05:00","adjustment":"已成交高敏感花材优先进场，代录批次提前30分钟到场"}'
# broker01：收到通知并确认/反馈异议
curl -s $BASE/api/notifications -H "$AUTH"
curl -s $BASE/api/coldchain/adjustments/2/confirm -X POST -H "$AUTH" -H 'Content-Type: application/json' -d '{"accept":true,"note":"已知悉，提前到场"}'
# boss01 维护花材敏感度（决定库位不足时的优先入库顺序）
curl -s $BASE/api/coldchain/sensitivity -H "$AUTH"
curl -s -X PUT $BASE/api/coldchain/sensitivity/rose -H "$AUTH" -H 'Content-Type:application/json' -d '{"sensitivity":5,"temp_window":"2-4℃ 优先"}'
```

## 异常场景与系统行为对照

| 场景 | 系统行为 |
|---|---|
| 开放度不均 / 短枝比例过高 | 质检录入均匀度<0.7、短枝率>0.2 自动打风险标；D 级批次打回不得上拍 |
| 冷库库位不足 | 入库时校验库位余量，不足返回 409「冷库库位不足」；凌晨爆满自动进入**库位不足调度**：按花材敏感度→已成交→预计提货时间排队，登记临时保温方式与损耗责任，看板实时展示占用与等待队列 |
| 临时保温失败、花材开放过快 | 上报 `cold_chain_incidents`，**冷库负责人与批发商双方分别确认责任**（非拍得批发商无权确认）；双方确认齐后**冷库端同步复盘**，复盘后市场管理方调整该时段入场顺序并通知经纪人，**经纪人确认/反馈异议** |
| 花店超时未付款 | 清扫器到期自动取消订单、释放锁定库存，状态「超时取消」 |
| 批发商临时改价 | 改价写入 `price_changes` 留痕；未付款订单仍按下单时锁定价 |
| 节日前订单被拆分 | `POST /api/orders/{id}/split` 拆出子单（继承价格/状态），各自配送结算 |
| 花材到店后掉瓣 | 送达后花店发起退货（petal_drop+损耗比例），质检裁定后计入赔付与结算 |
| 账期客户逾期 | 应收到期自动转逾期；结算时按日罚息计入「账期扣款」（封顶 20%） |
| 退货判定花农责任 | 扣减花农信用分（每 10% 损耗扣 1 分），竞价大厅按信用分重排 |

## 结算口径（`POST /api/settlements/generate {"month":"YYYY-MM"}`）

- **花农**：净额 = 竞价成交收入 − 退货扣款（判责部分）− 未售出批次冷库费。
- **批发商**：净额 =（销售成交 − 竞价采购成本）− 退货赔付 − 在库批次冷库费。
- **花店/婚庆**：应付 = 采购总额 − 退货返还 + 账期逾期罚息。
- 冷库费 = 入库时长（stored_at → 出清/当前）× 扎数 × 费率（默认 0.2 元/扎/小时）。
- 每份结算单含逐笔明细行（成交/退货/冷库费/罚息），可追溯原单。

## API 一览（全部 JSON，`Authorization: Bearer <token>`）

- 认证：`POST /api/auth/login`、`POST /api/auth/logout`、`GET /api/auth/me`、`GET /api/farmers`
- 批次/质检/冷库：`POST|GET /api/batches`、`GET /api/batches/{id}`、`GET /api/qc/pending`、`POST /api/batches/{id}/qc`、`GET /api/storage/slots`、`POST /api/batches/{id}/store`、`GET|POST /api/preservatives`、`POST /api/batches/{id}/preservative`
- 竞价：`POST|GET /api/auctions`、`GET /api/auctions/{id}`、`POST /api/auctions/{id}/bid|buy-now|close`
- 库存/调货：`GET /api/inventories`、`GET /api/market`、`POST /api/inventories/{id}/listing|price`、`GET /api/price-changes`、`POST|GET /api/transfers`
- 订单：`POST|GET /api/orders`、`GET /api/orders/{id}`、`POST /api/orders/{id}/pay|cancel|split`
- 配送：`POST|GET /api/deliveries`、`POST /api/deliveries/{id}/status`
- 退货：`POST /api/orders/{id}/returns`、`GET /api/returns`、`POST /api/returns/{id}/review`、`GET /api/returns/reconciliation`
- 财务：`GET /api/receivables`、`POST /api/receivables/{id}/settle`、`POST /api/settlements/generate`、`GET /api/settlements`、`GET /api/settlements/{id}`
- 看板：`GET /api/analytics/overview`、`GET /api/analytics/stability`、`GET /api/farmers/ranking`
- 冷库库位不足调度：`GET /api/coldchain/board`、`GET /api/coldchain/sensitivity`、`PUT /api/coldchain/sensitivity/{category}`、`GET|POST /api/coldchain/queue`、`POST /api/coldchain/queue/{id}/admit|release`、`GET|POST /api/coldchain/incidents`、`POST /api/coldchain/incidents/{id}/confirm|review|adjustment`、`GET /api/coldchain/adjustments`、`POST /api/coldchain/adjustments/{id}/confirm`
- 通知：`GET /api/notifications`、`POST /api/notifications/{id}/read`、`POST /api/notifications/read-all`
- 其他：`GET /healthz`（健康检查）、`GET /`（单页前端）

## 环境变量

见 `.env.example`：`PAYMENT_TIMEOUT_MIN`（支付时限）、`PRESALE_PAYMENT_HOURS`（预售时限）、`SWEEP_INTERVAL_SEC`（清扫周期）、`STORAGE_FEE_PER_BUNDLE_HOUR`（冷库费率）、`CREDIT_DAILY_PENALTY_RATE`（逾期日罚息）、`SEED_DEMO`（演示数据开关）等。

## 本地开发（无 Docker，直连本机 PG/Redis）

```bash
# 1) 准备 PostgreSQL（建库建账号）与 Redis，例如：
createdb -U flower flowermarket            # 或用 docker 只起 db/redis
redis-server --daemonize yes --port 6379
# 2) 运行（首次自动建表并写入演示数据）
DATABASE_URL='postgres://flower:flower@127.0.0.1:5432/flowermarket?sslmode=disable' \
  REDIS_ADDR=127.0.0.1:6379 PORT=8080 go run .
# 3) 浏览器打开 http://localhost:8080
```

> 标准验证方式仍是仓库根目录 `docker compose up -d --build`（见「快速开始」）。本机无 Docker 时可用上面的直连方式。

## 本次实际验证记录（2026-09-27）

在无 Docker 的环境用 Go 1.26.8 编译、本机 PostgreSQL 15 + Redis 启动应用（自动迁移+种子），通过两段脚本完成验证：

- `go vet ./...` 无告警；`go build` 成功；内嵌前端 JS 语法解析通过。
- **冷库调度专项 45 项全部通过**：六库位爆满（各余 0-15 扎）；队列按「敏感度→已成交→预计提货时间」三级排序（玫瑰敏感5 居首、已成交百合先于未成交尤加利、同敏感度同成交态早提货优先）；未入库批次均带临时保温方式与损耗责任；爆满时直接入库与放行均返回 409；腾位后队首成功入库；保温失败事故由冷库与拍得批发商双方确认（非拍得批发商 403）、双方齐备前禁止复盘、冷库复盘留痕；复盘后市场发起入场调整并定向通知经纪人（无经纪人则通知全体）；经纪人确认/异议回写；重复入队、重复复盘、重复调整等幂等拦截；角色权限隔离（花店禁写、冷库不可改敏感度）；看板计数联动。
- **原有流程回归 26 项全部通过**：竞价/库存/市场/订单/配送/退货/财务/看板等只读接口正常；月度结算可生成；下单→支付→发车→送达→退货→质检裁定全链路通畅；前端首页含「冷库调度」「通知」入口与视图。
