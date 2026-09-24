# workbuddy2api 功能清单（intake 枚举，2026-09-24）

来源：`.scratch/refs/workbuddy2api` README「核心能力」+ cmd/ + internal/ 逐文件。
对照列 = 我们项目（sub2api codebuddy 平台，截至 v0.1.200）的对齐状态。

## A. 账号池治理
| # | 功能 | 上游实现 | 我们现状 |
|---|---|---|---|
| A1 | OAuth 设备授权登录（授权URL→轮询→落盘→热加载，无PKCE，多号连续添加） | cmd/login, internal/auth | ✗ 缺：我们靠管理面板扫码导入（admin.codebuddy.qr），无设备授权流 |
| A2 | 三因子加权随机选号（credits×10+快过期×8+闲置补偿；Top-5短名单抽签+LRU兜底） | internal/pool/pick.go | ✗ 缺：我们按 load/粘性/优先级，无积分权重 |
| A3 | 防惊群（100ms 内刚选中账号跳过） | pool | ✗ 缺 |
| A4 | 在途租约（单号最大在途上限，占满不选） | pool.max_in_flight | ◐ 部分：账号 concurrency=5，但无「占满即出候选」语义对齐记录 |
| A5 | 账本择优（(账号,模型)每千token单价台账，EMA平滑，6h失效，落盘，/status透出） | upstream/growth_bonus 等 | ✗ 缺：我们无单价账本 |
| A6 | 成本分层条件探索（tier0免费/tier1未知/tier2收费；搭车改道探索≤48次/天/模型；毕业机制） | pool/pick | ✗ 缺 |
| A7 | 分级熔断与冷却（429软冷却600s指数退避封顶；404浅冷却；402/余额尽硬冷却至次日04:00；连败熔断封顶6h） | pool/cooldown,transition | ◐ 部分：6004已做（本会话）；402/404/连败熔断未对齐 |
| A8 | 6004 模型级限流独立冷却 + /status 台账 | pool | ✅ 已对齐（v0.1.200，本会话）；台账透出缺 |
| A9 | 账号临时停用/恢复（manual_disabled 与 disabled 双状态位独立；停用期签到保活照常；admin端点+CLI） | server/admin.go, cmd/acct | ✗ 缺：我们停调=不可调度，无「摘出对话流量但保活照常」语义 |
| A10 | 池状态持久化（state.json 原子落盘 + 可选 Upstash Redis 镜像，重启择优恢复） | pool/persist, redisstore | ✗ 缺：我们的冷却/熔断状态在DB，无镜像 |

## B. 请求链路
| # | 功能 | 上游实现 | 我们现状 |
|---|---|---|---|
| B1 | 出站强制 stream:true + SSE 白名单重建 + 非流式本地聚合 | upstream/sse.go | ✅ 已有（raw chat completions 透传+聚合） |
| B2 | DeepSeek 思维链注入（thinking.enabled+档位，reasoning_content 回填，effort 降级） | upstream/thinking.go | ◐ 部分：payload_rules 有 developer 归一；thinking 注入/回填需核对 |
| B3 | 系统提示词三模式（passthrough/custom/append + 拦截降级重试 + prompt.file） | internal/prompt | ✗ 缺 |
| B4 | 会话头族注入（X-Conversation-Request-ID 聚合主键·X-Conversation-ID 透传·B3链路；轮转/重试复用同键） | upstream/headers.go | ✅ 已对齐（codebuddy_upstream_identity.go，v0.1.190） |
| B5 | 指纹脱敏（出站请求体黑名单字段清洗，可开关） | upstream/sanitize.go | ◐ 部分：我们有 payload_rules 清洗，覆盖面需对照 |

## C. 选号语义
| # | 功能 | 我们现状 |
|---|---|---|
| C1 | 成本分层选号串联（粘性→分层→加权；tier1 不跳过；观测6h过期自动跟随限免） | ✗ 缺（依赖 A5/A6） |
| C2 | 会话粘性键优先级（conversation四键→prompt_cache_key→首条user sha256兜底；user_id 不作粘性键；30min滚动续期） | ✅ 基本对齐（openai_gateway_scheduling 优先级一致；TTL 1h vs 30min 差异） |
| C3 | 负载分布摊开（加权随机是概率倾斜非硬排序） | ✗ 缺（依赖 A2） |

## D. 定时积分任务（六类独立排程独立开关）
| # | 任务 | 上游时点 | 我们现状 |
|---|---|---|---|
| D1 | 签到+余额查询，余额恢复自动解冻冷却账号 | 09/21点 | ◐ 签到排程已有（195-200）；「余额恢复自动解冻」缺 |
| D2 | 活跃地图（连发上报点亮+连登+补签卡+档位兑换+抽奖+礼包补偿+回读streak自检） | 10点 | ◐ 活跃上报已有；补签卡/兑换/抽奖/礼包未吸收 |
| D3 | 猫猫旅行（领养/派出/领奖闭环） | 09/21点 | ✅ 已有（codebuddy_growth_channel_travel.go） |
| D4 | token 保活（全号刷新；session失效连续3次才禁用） | 22点 | ✗ 缺 |
| D5 | 开学季任务（点亮+claim+自动抽空抽奖余额；下线自动跳过） | 12点 | ◐ school.go 对应我们 scheduler/school？需核对吸收度 |
| D6 | 夜猫子任务（23:00-08:00 CST 补 black_cat） | 01点 | ◐ night_cat 已有（D4b 未测） |

## E. 双域适配
| # | 功能 | 我们现状 |
|---|---|---|
| E1 | 国内(CN)+国际(Global)双域共享账号池，realm/模型前缀 cn:/global: 路由 | ✗ 缺（登记过 global realm 未验证） |
| E2 | 国际版注册激活、地区完善、trial 加油包领取 | ✗ 缺（trial 注释失准已登记） |

## F. 辅助工具/可观测
| # | 功能 | 我们现状 |
|---|---|---|
| F1 | 积分日报 credit.sh（美化/-json，realm感知） | ✗ 缺 |
| F2 | 手动批量签到 signin.sh（幂等） | ◐ 面板手动签到已有；批量CLI缺 |
| F3 | 账号停用/恢复 CLI acct.sh | ✗ 缺（依赖 A9） |
| F4 | 领养联动/任务查询 task_runner.py（dry-run 默认） | ◐ 部分对应 |
| F5 | /healthz 语义（healthy/total/service 字段） | ◐ 我们有 healthz，字段不同 |
| F6 | /status 台账透出（model_costs/cost_explore/rate_limited_models 等） | ✗ 缺 |
| F7 | admin.enabled 显式开启的管理端点族 | ◐ 我们有 admin API，语义不同 |
| F8 | metrics / WAF IP / 退避降级组件 | ✗ 缺/需核对 |

## 结论
已对齐：A8、B1、B4、C2、D3（5 项）
部分：A4、A7、B2、B5、D1、D2、D5、D6、F2、F4、F5、F7（12 项）
缺失：A1、A2、A3、A5、A6、A9、A10、B3、C1、C3、D4、E1、E2、F1、F3、F6、F8（17 项）
共 34 项。disposition 逐项裁定在 design-draft 阶段完成。
