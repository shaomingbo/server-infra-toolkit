# PRD: T6 部署加固(运行时基线验证 + 成本告警 + 部署链护栏)

## 1. Summary

给已在 Cloud Run 线上运行的服务补齐"稳态运维基线":把部署运行时从"能跑"升级为"已验证"(startup probe 配置、关停时间预算对账、清偿历史 DEFERRED 线上验收欠债),给账单加被动告警层(GCP budget 三档阈值触达邮箱),给手动部署链加只读前置校验与正式的主线外验证路径(蓝绿候选文档化)。全程守"做小":零新依赖、零自动化部署、零新环境、零坐标泄露。使用者是明博本人(单人开发 + AI 协作)。Mode:**L1**(五件套并行批判)。

## 2. Goal Alignment

- **Target user / operator**:明博(单人开发)的部署运维面;服务自身的运行时稳态。无外部最终用户。
- **Problem**:服务上线后部署链停留在 T0 最小闭环:Cloud Run 探针零配置(全靠隐式默认)、代码侧关停预算从未与 Cloud Run 宽限期对账、DEPLOY.md §9 双缩零验收至今标 DEFERRED 未线上验证、账单超支无任何主动通知(只有 max-instances=2 硬护栏)、手动 runbook 的高频易错步骤(IAM 授权/secret/镜像/DSN)无前置拦截。
- **Success outcome**:部署运行时基线可验证 + 账单超阈值主动触达邮箱 + 部署前置校验脚本拦住四类高频配置错误 + 主线外验证有正式文档路径;全程不引入常驻进程/IaC/自动化部署/新环境。
- **Scope boundary**:做 = probe 配置、关停对账、欠债验收、budget alert、只读校验脚本、蓝绿候选文档化;不做 = 持久 preview 环境(defer 客户端联调)、Neon 自动告警、liveness probe、IaC、CD 自动化、APM、多 region。
- **First acceptance signal**:部署前置校验脚本在故意拔掉 IAM secretAccessor 授权的环境下非零退出并指明缺失项。
- `goal_alignment.status = clear`(roadmap §7.1 T6 行 + 明博在 T5 收官对话中拍板 T6 先行的理由直接点明目标;G3 preview 的 scope 取舍经一次选择题由明博锁定,见 D1)。

## 3. Background and Problem

T0–T3 与 T5 已落地,服务跑在 Cloud Run scale-to-zero(max-instances=2)+ Neon。部署链现状:`scripts/build.sh` 推镜像 + `docs/DEPLOY.md` 手动 runbook + `scripts/smoke.sh` 部署后冒烟 + GitHub Actions 只跑 `verify.sh`。三类欠账:(1) **运行时配置欠账**——Cloud Run 无任何 probe 配置,`cmd/api/main.go` 注释明示"完整连接 draining 推迟到 T6"(NG9),排空预算(HTTP 5s + 池 2s)与 Cloud Run SIGTERM 宽限期的关系从未对账;(2) **验收欠账**——DEPLOY.md §9 双缩零 L2 验收标 DEFERRED,retry.go 的冷启动行为线上未确认;(3) **防线欠账**——账单只有实例数硬护栏,无被动告警层;runbook 最易错的四类配置错误(IAM 403/secret 缺失/镜像未推/DSN 缺 sslmode)靠冒烟事后兜底。

## 4. Users and Use Cases

- **主用户**:明博(部署操作者 + 账单责任人)。
- **次级**:实现/审查 AI 代理(消费 runbook 与校验脚本)。
- 用例:
  1. 部署新版本前跑前置校验脚本,IAM/secret/镜像/DSN 四项任一缺失立即被拦,不必等部署后冒烟失败再排查。
  2. 部署候选 revision 到独立 URL 冒烟,确认后切流量;出问题按演练过的回滚步骤回退。
  3. 月账单跨过 50%/90%/100% 阈值时收到邮件,不必等月底账单。
  4. 服务缩零后被唤醒,startup probe 给冷启动留足余量,实例不被误杀。
  5. 换 revision 时旧实例在宽限期内排空 in-flight 请求,慢请求拿到完整响应而非被截断。

## 5. Goals and Non-goals

**Goals**:
1. 部署运行时基线从"能跑"变为"已验证":probe 配置、关停预算对账、双缩零历史欠债清偿。
2. 账单异常从"月底才知道"变为"阈值触达邮箱"(云侧被动,零代码)。
3. 部署 runbook 高频错误有前置拦截(只读校验脚本)+ 主线外验证有正式路径(蓝绿候选文档化)。
4. 全程守做小:零新 Go 依赖、零自动化部署、零新环境、零坐标泄露。

**Non-goals**(逐条钉死):
- 不建持久 preview 环境(第二 Cloud Run service + Neon branch)。**触发条件**:客户端 app-infra-toolkit 联调需求出现时按真实形状另行 forge(D1)。
- 不做 Neon 用量自动告警/轮询——会逼出常驻/定时任务(撞 NFR1)+ 新 secret,降级为 runbook 手动检查(D2)。
- 不配 liveness probe——配错 = 实例循环杀,本站只配 startup probe(D4)。
- 不做 IaC(Terraform/Pulumi),不引入 service.yaml 声明式部署路径(D7)。
- 不做 CD/部署自动化;`gcloud` 写操作(deploy/secret/IAM)维持"刻意不进脚本"(D6/D9)。
- 不做 APM/分布式 tracing/多 region/告警分级/on-call 体系。
- 不新增主动探测(任何周期性请求服务的任务,防唤醒缩零实例烧钱)。
- 不动 config 机制(不加 APP_ENV/profile/新 secret/日志 env 字段)(D8)。

## 6. Requirements

- **FR1 (must)**:Cloud Run 配置 **startup probe** 指向 `/livez`(绝非 `/healthz`,GCP 边缘保留),参数(initialDelay/timeout/period/failureThreshold)在 runbook 给出数值与冷启动余量论证;**liveness probe 本站不配置**,进程挂死的发现手段缺位记录为已接受风险。
- **FR2 (must)**:关停时间预算对账:(a) 排空逻辑自动化测试——人为延迟请求在 shutdown 触发后仍收到完整响应、新请求被拒、shutdown 在预算内返回(允许把 `main.go` 关停块抽为可测函数,行为不变);(b) 预算锚定断言——HTTP 排空预算 + 池关闭预算之和 ≤ runbook 声明的 Cloud Run 宽限期数值。NG9"完整连接 draining"收口为本条对账,不做主动编排(D5)。
- **FR3 (must)**:清偿 DEPLOY.md §9 双缩零 DEFERRED 验收:线上实走一次(服务与 Neon 同时缩零后唤醒),冷启动首请求时延与 `db_retry` 日志观测数值回写 runbook,状态改为含日期的已验证记录。
- **FR4 (must)**:GCP Billing budget:月预算 + 50%/90%/100% 三档阈值,通知可达明博邮箱(一次性测试通知确认);runbook 含创建命令模板,billing account id 用占位符。
- **FR5 (must)**:Neon 用量手动检查:runbook 新章节(控制台入口路径 + 关注指标清单 + 红线数值),零代码、零轮询、零新 secret。
- **FR6 (must)**:部署前置校验脚本(`scripts/`,只读幂等):检查 ① 运行时 SA 的 secretAccessor 绑定 ② secret 存在且最新版可读 ③ 镜像 tag 已推 ④ DSN 含 `sslmode=require`;任一缺失退出码非 0 并指明缺失项;**不进 verify.sh、不进 CI**(与 build.sh/smoke.sh 同属手动部署链)。
- **FR7 (must)**:蓝绿候选机制(`--no-traffic --tag candidate`)文档化为正式"主线外验证"章节(候选部署 → 候选 URL 冒烟 → 切流量 → 回滚各步命令可照抄);持久 preview 显式 defer(见 Non-goals)。
- **FR8 (should)**:回滚链一次实走演练(部署候选 → 切流量 → 回退到前一 revision),结果(日期 + revision 名)回写 runbook。
- **FR9 (must)**:`docs/CONTRACTS.md` append T6 范围声明:工件落点(scripts/+docs/,无新第一层目录)、无新 env var/端点/secret、verify.sh 仍是唯一 CI 入口、部署校验脚本属部署链非 CI gate。
- **NFR1 (must)**:无主动探测——不新增任何周期性请求服务的任务(Cloud Scheduler/cron/CI 定时),监控只用云厂商被动侧数据。
- **NFR2 (must)**:坐标零泄露——真实 GCP 坐标(project number/region/服务域名/billing account)只存在于本地 `.env`;T6 落地后全仓 grep 无命中,文档一律占位符。
- **NFR3 (must)**:冻结契约不破——第一层目录树不变、config 机制零改动、`/livez` 无 DB 守卫不破、verify.sh 唯一 CI 入口、`gcloud` 写操作不进脚本。
- **NFR4 (must)**:做小纪律——无 IaC、无 CD workflow、无第二 Cloud Run service、无新 secret、无新 Go 依赖。

## 7. User Flow or State Flow

部署生命周期(T6 加固后):

```
前置校验脚本(四项只读检查,任一缺失即停)
  → build.sh(镜像)→ goose up(迁移,独立步骤)
  → gcloud run deploy --no-traffic --tag candidate(手动,照 runbook)
  → 候选 URL 冒烟(smoke.sh)
  → 切流量 → 部署后冒烟
  → 异常 → 回滚(演练过的步骤,FR8)
```

运行时状态:缩零 → 请求唤醒 → 冷启动(startup probe 余量内)→ serving → SIGTERM(换 revision/缩容)→ HTTP 排空(≤5s)→ 关池(≤2s)→ 退出(总和 ≤ 宽限期)。
监控面:budget 配置态 → 跨阈值 → 邮件触达(被动,滞后于账单数据延迟,见 R2)。

## 8. Data, API, Permissions

- **无新端点、无 schema 变更、无新 secret、无日志字段变更、config 零改动**。
- 涉及 GCP 资源(全部控制台/gcloud 手动,模板进 runbook):Cloud Run 服务的 startupProbe 字段与 terminationGracePeriodSeconds(读 + 手动改);GCP Billing budget(账户级,创建);IAM policy(只读校验)。
- 校验脚本只调只读 API(`gcloud ... describe/get-iam-policy`、`docker manifest inspect` 或等价)。
- 产出分类(防"文档写全了"假绿):代码层(FR2)→ automated_test 进 verify.sh;GCP 配置层(FR1/FR4)→ manual_check + runbook 留痕;文档层(FR5/FR7/FR9)→ code_review;实操层(FR3/FR8)→ manual_check + 回写 runbook。

## 9. Acceptance Criteria

- **AC1**:Given Cloud Run 已按 runbook 配置指向 `/livez` 的 startup probe,when 服务缩零后连续 3 次冷启动发首请求,then 每次首请求返回 200,`gcloud run revisions list` 无 revision 处于非 Ready 或重启循环状态,且 probe path 字段值等于 `/livez`。
  Verifiable by: manual_check;Refs: FR1
- **AC2**:Given 关停排空逻辑,when 自动化测试发起一个人为延迟大于等于 3 秒的请求并随即触发 shutdown,then 该请求收到完整 200 响应、shutdown 调用在声明预算内返回、shutdown 开始后的新请求被拒绝。
  Verifiable by: automated_test;Refs: FR2
- **AC3**:Given 代码侧关停预算常量,when 运行预算锚定测试,then HTTP 排空预算与池关闭预算之和小于等于 10 秒(runbook 声明的 Cloud Run 宽限期数值),改动任一常量该测试变红。
  Verifiable by: automated_test;Refs: FR2
- **AC4**:Given DEPLOY.md §9 标 DEFERRED 的双缩零验收,when 按 runbook 在线上实走一次,then §9 更新为含日期、冷启动首请求时延数值、`db_retry` 日志条目观测结果的已验证记录,DEFERRED 标记消失。
  Verifiable by: manual_check;Refs: FR3
- **AC5**:Given 按 runbook 模板创建的 GCP budget(50%/90%/100% 三档),when 发送一次测试通知,then 明博邮箱收到该通知,且 runbook 模板中 billing account id 为占位符。
  Verifiable by: manual_check;Refs: FR4
- **AC6**:Given runbook 的 Neon 用量检查章节,when 审查该章节与全仓代码,then 章节含控制台入口路径、指标清单、红线数值三要素,且仓库无 Neon API key 引用、无周期性用量拉取代码。
  Verifiable by: code_review;Refs: FR5
- **AC7**:Given 部署前置校验脚本,when 在四项配置各自故意置缺的环境下分别执行,then 每次退出码非 0 且输出指明缺失项;四项齐备时退出码 0。
  Verifiable by: manual_check;Refs: FR6
- **AC8**:Given 校验脚本落地,when 审查 verify.sh 与 `.github/workflows/`,then verify.sh 不引用该脚本,workflows 目录无新增文件与新增步骤。
  Verifiable by: code_review;Refs: FR6, NFR3
- **AC9**:Given runbook 的主线外验证章节,when 照章节命令执行一遍(候选部署不切流量 → 候选 URL 冒烟 → 切流量 → 回滚),then 每步命令可照抄执行,回滚后线上 `/livez` 返回的 version 等于前一 revision 的 SHA,演练结果(日期 + revision 名)回写 runbook。
  Verifiable by: manual_check;Refs: FR7, FR8
- **AC10**:Given T6 全部落地,when 审查仓库,then CONTRACTS.md 含 T6 范围声明节、第一层目录集合与 §1.1 冻结清单一致、`internal/platform/config/` 无改动、按 runbook 给定的 grep 模式全仓无真实 GCP 坐标命中。
  Verifiable by: code_review;Refs: FR9, NFR2, NFR3
- **AC11**:Given T6 落地,when 审查仓库与 GCP 项目,then 无新增周期性请求服务的任务、无新增 GitHub workflow、无 Terraform/Pulumi 文件、无第二个 Cloud Run service、go.mod 无新增依赖。
  Verifiable by: code_review;Refs: NFR1, NFR4
- **AC12**:Given startup probe 配置完成,when 审查 Cloud Run 服务配置与 runbook,then liveness probe 字段未设置,且"进程挂死的发现手段缺位"作为已接受风险记录于 runbook。
  Verifiable by: code_review;Refs: FR1

## 10. Edge Cases and Failure States

- **E1** probe 参数过紧,冷启动期实例被判不健康:runbook 参数必须附余量论证;AC1 三连冷启动验证兜底。
- **E2** budget 通知发给 billing admin 默认渠道而非明博常用邮箱:测试通知步骤(AC5)暴露此问题,修正通知渠道后重验。
- **E3** 校验脚本在无 gcloud 凭据的机器上执行:输出指明"凭据缺失"并非零退出,不得误报为配置缺失。
- **E4** 回滚演练时服务只有一个 revision(无可回退目标):runbook 标注演练前提(至少 2 个 revision)。
- **E5** 双缩零验收撞上 Neon 维护窗口或异常波动:记录原因并重跑,不把失真数值写入 runbook。
- **E6** 坐标 grep 自查误报(占位符/文档示例命中):grep 模式只匹配真实值形态(数字 project number、真实 region 字符串),runbook 注明白名单。

## 11. Risks and Mitigations

- **R1** liveness 不配,进程挂死(死锁/活锁)无自动发现。impact: medium / likelihood: low。Mitigation: Cloud Run 对 crash 自动重启 + 缩零天然轮换实例;作为已接受风险记录(D4)。Owner: engineering
- **R2** budget alert 滞后于 GCP 账单数据延迟(24h+),单日异常烧钱发现偏晚。impact: medium / likelihood: low。Mitigation: 实时防线是 max-instances=2 与 T5 请求体/条数硬上限,alert 定位为事后知情层(D3)。Owner: Mingbo
- **R3** GCP 侧手工配置与 runbook 声明漂移(改了控制台没改文档)。impact: low / likelihood: medium。Mitigation: runbook 附 `gcloud ... describe` 只读对账命令,部署检查清单含一次对账。Owner: ops
- **R4** 部署仍手动,人为抄错 runbook。impact: medium / likelihood: medium。Mitigation: 前置校验脚本拦四类高频错 + 蓝绿候选先冒烟再切流量 + 回滚演练过(D9 显式接受)。Owner: Mingbo
- **R5** defer 持久 preview,客户端联调启动时临时搭环境仓促。impact: low / likelihood: medium。Mitigation: D1 记录触发条件,客户端接入 session 自带 forge,届时按真实需求形状设计。Owner: Mingbo

## 12. Default Decisions

- **D1**:G3 preview 按明博选定锁定——复用现有蓝绿候选机制并文档化,持久 preview(第二 service + Neon branch)defer。Why: R3 选择题明博选定;现有候选机制覆盖主线外验证大部分诉求,持久 preview 四重新风险(孤儿 branch 计费/配置漂移/攻击面/坐标泄露)无现时收益。Override if: 客户端 app-infra-toolkit 联调需求出现,另起 forge。
- **D2**:G1 拆分——GCP budget alert 做,Neon 用量自动告警不做(降级 runbook 手动检查)。Why: 自动化需轮询任务 + 新 secret,撞 NFR1 无常驻进程与 T7 backlog 边界。Override if: Neon 当前 plan 提供原生阈值推送,则改为控制台配置(仍零代码)。
- **D3**:告警验收从"天级发现异常"降为"三档阈值通知可达邮箱"。Why: GCP 账单数据自身延迟 24h+,端到端发现时延无法承诺,写进验收就是假承诺。Override if: 无。
- **D4**:只配 startup probe,不配 liveness probe。Why: liveness 配错 = 实例循环杀(爆炸半径大于收益);挂死场景有 crash 自动重启 + 缩零轮换兜底。Override if: 线上出现真实挂死事故。
- **D5**:NG9"完整连接 draining"收口为预算对账 + 排空自动化测试,不做主动连接编排。Why: HTTP 先排空 + pgxpool.Close 阻塞归还已覆盖正常路径,主动编排无明确增量收益。Override if: FR2 测试发现 in-flight query 被截断。
- **D6**:G4 限只读前置校验;`gcloud` 写操作维持"刻意不进脚本"。Why: DEPLOY.md 既有有意决策(高危操作人在回路),公开仓库下尤其如此。Override if: 无。
- **D7**:T6 工件全落 `scripts/` + `docs/`,不开新第一层目录,不引入 service.yaml 部署路径(期望值写 runbook + describe 只读对账)。Why: §1.1 目录树冻结 + 做小;声明式文件与命令式部署并存会产生双真相漂移。Override if: 部署工件类增多到散乱,走受控演进出口开 `deploy/`。
- **D8**:不加 APP_ENV/日志 env 字段,config 零改动。Why: D1 选定后无环境标识需求;没有明确收益的复杂度默认拒绝。Override if: 持久 preview 落地时(届时新 env var 经 os.Getenv 是纯 append,不违宪)。
- **D9**:接受部署保持手动,T6 只加护栏不加自动化。Why: 部署自动化 = 过早平台化,违单人节奏 + 保守域;红队"all-options-wrong"分析的显式回应——把"手动部署 + 护栏"从隐性现状变为显式决策。Override if: 部署频率显著上升或转多人协作。

## 13. Open Questions

(无——G3 已经 R3 选择题消化为 D1,其余决策均 R1 default。)

## 14. Implementation Tasks

- **T1**:关停排空可测化(允许将 main.go 关停块抽为行为不变的可测函数)+ 排空自动化测试 + 预算锚定断言;verify.sh 全绿。Owner: implementer;Deps: [];Done when: AC2, AC3
- **T2**:部署前置校验脚本 `scripts/deploy-precheck.sh`(四项只读检查,凭据缺失与配置缺失输出区分);不接入 CI。Owner: implementer;Deps: [];Done when: AC7, AC8
- **T3**:runbook 大修(startup probe 模板与参数论证、宽限期声明、蓝绿候选正式章节、Neon 用量章节、liveness 已接受风险、坐标 grep 自查模式)+ CONTRACTS.md T6 范围声明 append。Owner: implementer;Deps: [];Done when: AC6, AC10
- **T4**:GCP 实操——配置 startup probe + 三连冷启动验证 + 确认 liveness 未配(明博在协调者引导下照 runbook 执行)。Owner: main_coordinator;Deps: [T3];Done when: AC1, AC12
- **T5**:GCP 实操——创建 budget(三档)+ 测试通知确认可达。Owner: main_coordinator;Deps: [T3];Done when: AC5
- **T6**:线上实操——双缩零 DEFERRED 验收实走 + 观测数值回写。Owner: main_coordinator;Deps: [T3, T4];Done when: AC4
- **T7**:线上实操——蓝绿候选全流程演练含回滚实走 + 回写。Owner: main_coordinator;Deps: [T3, T4];Done when: AC9
- **T8**:终审——脚本不进 CI、无主动探测、无 IaC/新依赖/第二 service、坐标 grep、冻结契约逐项核验。Owner: reviewer;Deps: [T1, T2, T3];Done when: AC8, AC11
