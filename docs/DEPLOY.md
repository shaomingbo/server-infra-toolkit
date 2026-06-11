# DEPLOY — Cloud Run 部署 runbook

明博可照着一步步做的部署操作手册。覆盖 T0「一次真实部署冒烟」(PRD FR7 / AC7 / AC12 / AC13 / AC14)。

> Cloud Run 给每个 revision 注入的环境变量名为 `K_REVISION`(代码已对此兜底:无 ldflags 注入的 SHA 时回退到它)。
> 本文里的 `gcloud` 命令是给明博照做的文本模板,**不在任何脚本里自动执行**(含 secret / SA 等需谨慎的操作,刻意不进脚本)。
> 下面命令里的 `<REGION>` / `<PROJECT_NUMBER>` / `<PROJECT_ID>` / `<AR_REPO>` / `<IMAGE>` 是占位符,
> **真实部署参数见本地 `.env`(不入库,见 `.env.example` 的 `IMAGE_REPO` 格式)**;照抄前替换成真实值。

固定参数(占位符,真实值见本地 `.env`):

| 参数 | 值 |
| --- | --- |
| region | `<REGION>` |
| project | `server-infra-toolkit` |
| service 名 | `server-infra-toolkit` |
| 运行时 service account | `<PROJECT_NUMBER>-compute@developer.gserviceaccount.com` |
| 镜像仓库路径 | `<REGION>-docker.pkg.dev/<PROJECT_ID>/<AR_REPO>/<IMAGE>` |
| secret 名(Neon DSN) | `NEON_DSN` |

---

## 1. 前置 checklist(对应 PRD Q1 / E3)

动手前逐项确认,缺失项先补齐:

- [ ] GCP project 已选定(`gcloud config get-value project` 为 `server-infra-toolkit`)。
- [ ] Secret Manager 已建好存 Neon DSN 的 secret(secret 名 `NEON_DSN`),secret 值为完整 Neon 连接串。
- [ ] 运行时 service account(`<PROJECT_NUMBER>-compute@developer.gserviceaccount.com`)已授 `roles/secretmanager.secretAccessor`。
      未授权时运行时读 secret 会报 403 PermissionDenied(在 IAM 层,不在 Go 代码,最难自查——见 PRD E3),
      这也是冒烟失败的第一排查项。
- [ ] Neon DSN 含 `sslmode=require`(Neon 强制 TLS,缺了连不上——见 PRD E5);pooler 端点 vs 直连端点明确选一。
- [ ] Artifact Registry repo 就绪(Docker 仓库,路径
      `<REGION>-docker.pkg.dev/<PROJECT_ID>/<AR_REPO>`),且本地已 `gcloud auth configure-docker <REGION>-docker.pkg.dev`。

**上面四类高频配置错误(IAM 授权 / secret 可读 / 镜像已推 / DSN 含 sslmode)加上「迁移版本对账」共五项,有只读自动化拦截**——构建推镜像(第 3 节)后、部署(第 4 节)前跑一次:

```bash
./scripts/deploy-precheck.sh             # 检查当前 HEAD 对应的镜像 tag
./scripts/deploy-precheck.sh <git-sha>   # 或显式指定要部署的 tag(须与 build.sh 推的一致)
```

- 脚本**只读、幂等、绝不做任何 gcloud 写操作**(D6),属手动部署链(与 `build.sh` / `smoke.sh` 同列),**不进 `verify.sh`、不进 CI**(部署链 ≠ CI gate,见 CONTRACTS.md §8)。
- 退出码语义:`0` 五项全过;`1` 某项**配置缺失**(输出指明缺哪项,去补齐);`3` **凭据缺失**(gcloud 没装 / 未认证 —— 与配置缺失区分,见 PRD E3,先 `gcloud auth login` 再重跑)。
- 第 5 项(迁移版本对账)对账**云端 DSN 指向的生产库** goose 版本与 `db/migrations/` 最新序号,拦"代码部署了但 `goose up` 没跑"(§9.1 流程教训的落地)。唯一已知副作用:对**从未迁移过**的库,goose 查版本会自动建一张空的版本表(无业务影响,随后对账会大声报红引导跑第 6 节)。
- 坐标零硬编码:project 取自 `gcloud config`、运行时 SA 由 project number 推导、`IMAGE_REPO` 取自本地 `.env`,脚本里无任何真实坐标(NFR2)。

---

## 2. 部署前迁移步骤(对应 PRD AC9 / FR8 / NG7 / E2)

> **迁移是部署流程里独立于应用启动的一步**:服务镜像不承担 schema 演进,服务启动路径
> **不含**任何迁移调用(`cmd/api/main.go` 只惰性建连接池、不拨号、不跑 goose——已对启动路径核实)。
> schema 的演进在这里手工完成,**先于**部署新 revision。

**顺序铁律:先迁移,后部署服务。** 部署任何会用到新 schema 的 revision **之前**,先把迁移跑到最新:

```bash
cd tools
go tool goose -dir ../db/migrations postgres "$NEON_DSN" up
```

- 命令在独立的 `tools` module 内调用(goose 版本锁在 `tools/go.mod`,不全局安装、不污染主服务 module),
  故先 `cd tools`;`-dir` 用相对 `tools` 的路径 `../db/migrations`。子命令与回滚边界的完整规范见
  [`db/migrations/README.md`](../db/migrations/README.md)(本节只给部署运维视角的步骤,不重复那里的全部细节)。
- **凭据复用 `NEON_DSN`,经受控注入**:迁移端点用 Neon 直连(`NEON_DSN` 已是直连串),与运行时同一个 secret。
  `$NEON_DSN` 由操作者在本地 shell 受控注入(如从 Secret Manager 取出后导入环境变量,或由 `.env` 提供),
  **本文档绝不写明文连接串 / 密码**——和第 4 节运行时用 `--set-secrets NEON_DSN=NEON_DSN:latest` 注入是同一条「DSN 不进 repo / workflow / 文档」原则(PRD D2 / AC7)。
- 跑完用 `go tool goose -dir ../db/migrations postgres "$NEON_DSN" status` 确认已应用到预期版本,再进第 3 节构建、第 4 节部署。

**为何与应用启动解耦(NG7 / E2)**:Cloud Run `min-instances=0` 缩零后,首请求可能同时唤起**多个**实例。
若把迁移塞进服务启动路径,这些并发实例会**竞争**同一份迁移(抢 goose 版本表锁)并**阻塞首请求**——
首个用户请求要等迁移跑完才返回,且并发迁移本身有死锁 / 半应用风险。把迁移提到部署前的独立一步,
启动路径就只剩「惰性建池」,首请求不被 schema 演进拖累。

---

## 3. 构建并推镜像

```bash
./scripts/build.sh            # tag = git rev-parse --short HEAD
./scripts/build.sh <git-sha>  # 或显式指定 SHA
```

脚本封装 `docker build` + `docker push`(不含 deploy),完成后会 echo 推送的完整镜像引用,记下它备部署用。

- **`--platform linux/amd64` 必加(脚本已内置)。** 开发机是 Mac arm64,Cloud Run 只跑 amd64;
  不加这个 flag,镜像架构不匹配,部署后容器起不来。
- **`GIT_SHA` 必传(对应 PRD E4,脚本已内置)。** 若漏传:`main.version` 退回 `dev`,运行时再退回 `K_REVISION`,
  线上 `version` 会变成 Cloud Run 的 revision 名而不是 git SHA,冒烟断言 ②(`version == git SHA`)会失败。

---

## 4. 部署新 revision

> **关键差异:首次部署 vs 后续部署。**
> `gcloud run deploy` **创建新服务时不支持 `--no-traffic`**(会报错 `not supported when creating a new service`)。
> 所以首次只能直接 100% 部署;**服务已存在后**才能用 `--no-traffic --tag candidate` 做蓝绿。

两种路径共用的固定 flag(均已实测):

- `--service-account <PROJECT_NUMBER>-compute@developer.gserviceaccount.com`:运行时 SA,让容器能读 secret(漏了运行时报 403)。
- `--allow-unauthenticated`:让冒烟能无 token 访问 `/livez`(漏了冒烟拿不到 200)。
- `--set-secrets NEON_DSN=NEON_DSN:latest`:Neon DSN 经 Secret Manager 注入,不以明文进 workflow/repo/Dockerfile(PRD D2 / AC7)。
- `--max-instances=2 --min-instances=0`:成本护栏,防失控账单(PRD D11)。`min-instances=0` 显式声明缩零。

### 4a. 首次部署(服务尚不存在 → 直接 100%,不能 --no-traffic)

```bash
gcloud run deploy server-infra-toolkit \
  --image <REGION>-docker.pkg.dev/<PROJECT_ID>/<AR_REPO>/<IMAGE>:$(git rev-parse --short HEAD) \
  --service-account <PROJECT_NUMBER>-compute@developer.gserviceaccount.com \
  --allow-unauthenticated \
  --set-secrets NEON_DSN=NEON_DSN:latest \
  --max-instances=2 \
  --min-instances=0 \
  --region <REGION>
```

首次部署直接接 100% 流量(无旧 revision 可保护,这是可接受的——见 PRD E1)。部署成功后记下服务 URL,
对它跑第 5 节冒烟。冒烟失败 → 这是「未切流量」前的唯一 revision,删服务或重部即可,无需回滚。

### 4b. 后续蓝绿部署(服务已存在 → --no-traffic --tag candidate 先冒烟)

```bash
gcloud run deploy server-infra-toolkit \
  --image <REGION>-docker.pkg.dev/<PROJECT_ID>/<AR_REPO>/<IMAGE>:$(git rev-parse --short HEAD) \
  --no-traffic --tag candidate \
  --service-account <PROJECT_NUMBER>-compute@developer.gserviceaccount.com \
  --allow-unauthenticated \
  --set-secrets NEON_DSN=NEON_DSN:latest \
  --max-instances=2 \
  --min-instances=0 \
  --region <REGION>
```

- `--no-traffic --tag candidate`:新 revision 部署但先不接生产流量,Cloud Run 给它一个带 `candidate` 标签的独立 URL(PRD AC13)。
- 对这个**候选 URL** 跑第 5 节冒烟,5 条全绿后再走第 6 节 `update-traffic --to-latest` 切流量。
- **本节是蓝绿候选的部署动作;完整的「主线外验证」闭环(候选部署 → 候选 URL 冒烟 → 切流量 → 回滚)整合在第 12 节,演练与回写也在那里**(T6 FR7)。

---

## 5. 冒烟 5 断言(对应 PRD AC12)

首次部署对服务 URL 跑、后续蓝绿部署对候选 URL(带 `candidate` 标签)跑,以下 5 条全绿才继续:

1. **revision Ready 且 serving**:`gcloud run revisions list --service server-infra-toolkit --region <REGION>` 中新 revision 状态为 Ready。
2. **/livez 200 且 version == SHA**:`bash scripts/smoke.sh <url>`。
   该脚本断言 `GET /livez` 返回 200 且 body 含 `version`,以及未知路由返回错误信封。
   再人工确认 body 里的 `version` 等于本次部署的 `git rev-parse --short HEAD`,而非 `dev` / revision 名。
3. **结构化日志条目**:手动查 Cloud Run 日志,确认刚才那次请求有结构化日志条目(含 `request_id` / `latency` / `version`)。
4. **Neon SELECT 1 成功**:对 Neon 跑一次性 `SELECT 1`(裸连接 + `SELECT 1` + 关闭,不建池——这是 T0 唯一 DB 交互)。
   - 本地:`go run ./cmd/api -smoke`(由 `.env` 提供 `NEON_DSN`)。
   - 容器内:`server -smoke`(运行时由 Secret Manager 注入 `NEON_DSN`)。
   - 成功打印 `neon smoke: ok` 并以退出码 0 结束;失败打印错误并退出码 1。该子命令只跑一次性探活后退出,**不起 HTTP server、不建连接池**。
   - 也可直接用 `psql "<Neon DSN>"` 执行 `SELECT 1;`。
5. **未知路由返错误信封**:`GET` 一个不存在的路由,响应体为约定错误信封 JSON(`error.code` + `requestId`)。
   (断言 ② 已含此项,这里再单列以对齐 AC12 的 5 条。)

5 条同时绿才算冒烟通过。任一红 → 走第 7 节回滚,标 T0 部署项未完成。

---

## 6. 通过才切流量(仅后续蓝绿部署)

> 首次部署已是 100% 流量,跳过本节。本节只针对第 4b 节的 `--no-traffic` 候选 revision。

```bash
gcloud run services update-traffic server-infra-toolkit --to-latest --region <REGION>
```

线上由新 revision 服务 → T0 部署项完成。

---

## 7. 回滚(冒烟失败时)

冒烟任一断言失败:**不切流量**,线上仍由旧 revision 服务,用户不受影响。

- 已有旧 revision(第 4b 节蓝绿部署):流量本就留在旧 revision(因为用了 `--no-traffic`);如需显式回切到指定 revision:
  ```bash
  gcloud run services update-traffic server-infra-toolkit --to-revisions=<prev>=100 --region <REGION>
  ```
- 首次部署、无旧 revision(对应 PRD E1):新服务已是 100% 流量,冒烟失败 → 删服务或修复后重部,无需"回滚"。

排查方向:PORT 未读 / IAM 403(secretAccessor)/ DSN 缺 sslmode / GIT_SHA 未传(见第 1、3 节与 PRD E2–E5)。

---

## 8. 代码回滚 ≠ schema 回滚(对应 PRD AC17 / NFR9 / E7)

> **回滚应用代码不会撤销已经在 Neon 跑过的迁移。** 两者是独立的回退动作,必须分开处理。

- **代码回滚**:`git revert` / 重部旧镜像只把**运行的代码**退回去。第 2 节跑过的迁移**已经落在 Neon 上**,
  `git revert` T1 代码**不会**反向跑那些迁移——schema 仍停在迁移后的版本,会与回退后的代码**脱节**。
- **schema 回退是独立动作**:真要退 schema,得另外手工跑 goose `down` / `down-to`,**不会**因为代码回滚而自动发生。
- **生产默认不退 schema,走前滚修复**:已发布的 schema 出问题,默认写一条**新迁移**(`00002_...`)把 schema 改对,
  而不是回退旧迁移(多实例 / 已有数据下回退风险高)。
- **破坏性 down(`DROP TABLE` / `DROP COLUMN` 等)标 `IRREVERSIBLE`、不入常规回滚流程**:语法上「可回滚」但数据
  不可逆丢失,只允许在本地 / 未发布、且明确接受数据丢失时执行。

回滚边界的完整规范(down 适用场景、IRREVERSIBLE 标注、「库 schema 与版本表不一致」恢复步骤)见
[`db/migrations/README.md` 的「AC7 四点规范 ③④」](../db/migrations/README.md#ac7-四点规范)——本节只给部署运维视角的结论,不重复全文。

---

## 9. 双缩零 L2 manual 验收 runbook(对应 PRD AC15 / NFR8 / D2 / E2)

> **状态:已验收(2026-06-11,T6 实操,验收记录见本节末)。** 首请求非 5xx 判据达成;重试链未在线上
> 观测到触发(原因见验收记录——Neon proxy 在唤醒期挂住连接而非快速报错,连接层失败未发生),重试保持
> 纵深防御待命,retry.go 注释中的冷启动 SQLSTATE 维持 UNCONFIRMED(真实触发一次后再回调参数)。

执行前提:服务已按第 1–6 节部署到 Cloud Run 且生产流量已切到该 revision(`min-instances=0`)。

手动验收步骤:

1. **让两层都缩零**:
   - **Neon**:停止对该库的所有访问,**等约 5 分钟** Neon compute 因 idle 自动缩零(Neon Scale to Zero 默认 5 分钟 idle 挂起)。
   - **Cloud Run**:同期不发任何请求,让实例自然缩到 0(`min-instances=0`)。可在 Cloud Run 控制台 / `gcloud run services describe`
     观察实例数归零。
2. **发首请求**:对一条**会走数据库**的请求路径(`Exec`/`Query`/`QueryRow`/`Begin`,**不是** `/livez`——它无 DB 依赖、不触发重试)
   发一次请求。这一请求会同时唤醒 Cloud Run 实例和 Neon compute。
3. **断言返回成功(而非 5xx)**:首请求应返回正常业务响应,**不是** 5xx。冷启动期的瞬时连接失败应被
   `retry.go` 的有界重试吸收并最终成功——这正是要验的「首请求从 5xx 变成功」。
4. **在日志看 slog 重试事件**:到 Cloud Run 日志查这次请求是否打出了双缩零重试的结构化事件:
   - `event: db_retry_attempt`(出现过瞬时连接失败、在退避后重试)——冷启动确实触发了重试。
   - `event: db_retry_succeeded`(重试后成功)——确认是「靠重试救回」而非「一把就连上」。
   - 若**没看到** `db_retry_attempt` 就直接成功,说明这次 Neon 唤醒够快、首次 acquire 就连上了;重试未被触发也算
     通过(重试是兜底),但应在多次缩零后重试中**至少观测到一次** `db_retry_attempt` + `db_retry_succeeded`,
     才算真正验到重试路径在线上生效。

验收通过判据:首请求返回成功(非 5xx),且至少一次缩零重试中观测到 `db_retry_attempt` → `db_retry_succeeded`
事件链。通过后据观测到的真实冷启动时延 / SQLSTATE 回调 `retry.go` 的 `totalBudget` 与 `retryableSQLSTATEs`(NFR8)。

### 9.1 验收记录(2026-06-11,T6 实操回写)

三轮双缩零观测(每轮先保证 Cloud Run 实例数归零、Neon idle ≥5 分钟,首请求用 `POST /v1/auth/login`
合法形状假凭据,真实走 `QueryRow` 链路):

| 轮次 | 结果 | 总时延 | 含义 |
|------|------|--------|------|
| 1 | 500 | 11.7s | 服务端零日志,无法定位 → 实锤 auth 500 路径可观测性缺口(修复:`ef071c5` 补 `auth_internal_error` 结构化日志) |
| 2 | 500 | 11.8s | 日志现形:`SQLSTATE 42P01`(users 表不存在)→ 实锤**生产库从未跑过迁移**(修复:`goose up` 至 version 3,六表对账齐) |
| 3 | **401(非 5xx)** | 12.4s | 标准信封 `unauthorized`,正确业务响应——**首请求非 5xx 判据达成** |

- 总时延剖面:约 10s 为 Cloud Run 实例冷启动(容器启动→startup probe 通过),handler 自身 0.6–2s。
- **重试链未观测到触发**(三轮 `db_retry_*` 事件均为零):实测 Neon proxy 在 compute 唤醒期**挂住连接等待**
  而非快速报错,连接层失败未发生,故步骤 4 预设的「冷启动必现瞬时连接失败」假设在当前 Neon 行为下不成立。
  重试逻辑保持纵深防御(Neon 行为变化 / 真实连接风暴时兜底),`retryableSQLSTATEs` 维持纸面值不回调。
- 流程教训(两条,均为本验收意外揪出):① 业务 5xx 必须有服务端结构化错误日志,否则线上黑盒;
  ② 「部署前 `goose up` 独立一步」(第 6 节)此前从未对生产库执行过——`/livez` 与错误信封冒烟均不碰业务表,
  掩盖了空库。后续部署务必执行第 6 节迁移步骤;deploy-precheck 已增加第 5 项「迁移版本对账」兜底(2026-06-11 落地)。

---

## 10. Startup probe 配置(对应 T6 FR1 / AC1 / AC12 / D4)

> **运行时基线加固:给 Cloud Run 显式配 startup probe,让缩零唤醒的冷启动有受控余量,实例不被默认探针误判。**
> probe 是 GCP 侧手工配置(`gcloud run services update` 的 flag,不进任何脚本——`gcloud` 写操作刻意人在回路);
> 期望值落本 runbook + 第 10.3 节 describe 只读对账,**不引入 service.yaml 声明式路径**(D7,避免双真相漂移)。

### 10.1 配 startup probe(指向 `/livez`,绝非 `/healthz`)

```bash
gcloud run services update server-infra-toolkit --region <REGION> \
  --startup-probe=httpGet.path=/livez,httpGet.port=8080,initialDelaySeconds=0,periodSeconds=10,timeoutSeconds=1,failureThreshold=6
```

- **path 必须是 `/livez`,绝不能是 `/healthz`。** GCP 边缘层保留了 `/healthz`,探针打 `/healthz` 会被边缘拦截(404),实例被判不健康进重启循环。`/livez` 是本服务无 DB 依赖的存活探针(缩零安全:Neon 睡着时探针不连库,不会因此杀实例),`livez_guard_test.go` 用 AST + `go list -deps` 强制它不引入 db 包。
- **port `8080`**:容器监听端口(`PORT` 缺省 8080;Cloud Run 注入 `PORT` 时与此一致)。

### 10.2 参数数值与冷启动余量论证(对应 E1)

| 字段 | 取值 | 理由 |
| --- | --- | --- |
| `initialDelaySeconds` | `0` | `/livez` 无 DB 依赖、进程一起来就能应答,不需要延迟首探;Go 服务监听端口即就绪。 |
| `periodSeconds` | `10` | 探测间隔,GCP 默认值;与 failureThreshold 相乘给出总余量(见下)。 |
| `timeoutSeconds` | `1` | 单次探测超时;`/livez` 是常数响应、本地不连库,1s 远够(约束:`timeoutSeconds` 不得超过 `periodSeconds`)。 |
| `failureThreshold` | `6` | **冷启动总余量 = `failureThreshold × periodSeconds` = 6 × 10 = 60 秒**。这是 Cloud Run 允许容器开始监听端口的窗口(硬上限 240s)。 |

**冷启动余量论证(为何 60s 而非默认 3×10=30s)**:本服务启动路径是"惰性建池、不拨号、不预热"(`db.NewPool` min conns 强制 0),进程本身秒级起来;但 Cloud Run 缩零唤醒包含拉取镜像 + 容器冷启动的平台开销,镜像较大或区域冷时偶发偏慢。`/livez` 不连库所以**不受 Neon 唤醒时延影响**——给到 60s 是对"镜像拉取 + 容器调度"平台抖动留的安全垫,远低于 240s 硬上限,也不会让真正起不来的实例卡太久。E1(参数过紧致重启循环)的兜底是 AC1 的三连冷启动验证:配完后缩零再唤醒三次,每次首请求 200、`gcloud run revisions list` 无 revision 处于非 Ready 或重启循环。

> **未配 startup probe 时 Cloud Run 的默认行为**(供对照):平台自动建一个默认 **TCP** startup probe(`timeoutSeconds=240, periodSeconds=240, failureThreshold=1`),即只检端口是否监听、给 240s。显式配 HTTP probe 打 `/livez` 比默认 TCP 探测多一层"HTTP 真能应答"的保证,且 60s 窗口更贴合本服务的真实冷启动剖面。

### 10.3 describe 只读对账(对应 R3 漂移检查)

GCP 侧手工改 probe 后,把配置与本 runbook 声明对账一次(R3:控制台改了文档没改会漂移),并纳入每次部署的检查清单:

```bash
gcloud run services describe server-infra-toolkit --region <REGION> \
  --format="yaml(spec.template.spec.containers[].startupProbe,spec.template.spec.containers[].livenessProbe)"
```

对账要点:① `startupProbe.httpGet.path` 字段值 **等于 `/livez`**(AC1);② 上述四个参数与第 10.2 节表格一致;③ **`livenessProbe` 字段为空/未设置**(AC12,见 10.4)。

### 10.4 liveness probe 不配 + 已接受风险(对应 FR1 / D4 / R1 / AC12)

> **本站只配 startup probe,显式不配 liveness probe。** 这是有意决策(D4),不是遗漏。

- **为何不配**:liveness probe 配错(阈值过紧 / 路径选错)= 实例被周期性判死并循环重启,爆炸半径远大于它的收益。
- **已接受风险(R1)**:**进程挂死(死锁 / 活锁,进程还活着但不再正常服务)的自动发现手段缺位。** 没有 liveness probe,这类"假活"实例不会被平台主动杀掉重建。
- **现有兜底**:① Cloud Run 对**进程崩溃(crash)** 自动重启;② `min-instances=0` 缩零会天然轮换实例(idle 后实例被回收,下次唤醒是新实例);③ startup probe 覆盖"起不来"场景。挂死场景只在"进程不崩、又长期不缩零"的窗口里裸奔,概率低(likelihood low)。
- **override 条件**:线上出现真实挂死事故时再评估配 liveness(D4 override_if)。

### 10.5 关停宽限期声明与排空预算对账(对应 T6 FR2 / AC3)

> **Cloud Run 关停宽限期 = 固定 10 秒,平台常量,不可配。** 实例缩容 / 换 revision / 任何 shutdown 时,Cloud Run
> 先发 `SIGTERM`,**10 秒**后发 `SIGKILL`。fully managed 形态**不暴露** `terminationGracePeriodSeconds` 配置入口,
> 也**不出现在** `gcloud run services describe` 的任何字段里——对账只能锚定本文档声明的常量(10s),不能从 describe 读。

- **代码侧排空预算必须 ≤ 10s**:`cmd/api` 关停先排空 HTTP in-flight 请求(预算约 5s)再关连接池(预算约 2s),两者之和 ≈ 7s,**留约 3s 安全垫给平台 SIGKILL 之前**。
- **两处同步纪律(改一处必改另一处)**:① 本 runbook 声明的宽限期数值(**10s**);② `cmd/api` 的关停预算锚定断言(FR2/AC3:HTTP 排空预算 + 池关闭预算之和 ≤ 10s,改任一常量该测试变红)。**runbook 的 10s 与代码锚定断言上限是同一个数,任一处变更必须同步另一处**,否则代码预算可能悄悄超过平台宽限期、in-flight 请求被 SIGKILL 截断。
- **注意 SIGTERM 是尽力而为**:exceptional cases 下 SIGTERM 可能发给仍在处理请求的容器;排空窗口只有 10s,**比 HTTP `WriteTimeout`(15s)短**——所以单个超长请求仍可能在关停时被截断,这是 10s 平台常量的固有约束,不是 bug。

---

## 11. Neon 用量手动检查(对应 T6 FR5 / AC6 / D2)

> **决策:Neon 用量只做 runbook 手动检查,不做自动告警 / 轮询(D2)。**
> 理由:自动化需要常驻 / 定时任务(撞 NFR1 "无主动探测"红线)+ 新 secret(Neon API key,撞"无新 secret")。
> 收益不抵复杂度与攻击面,降级为下面的手动检查清单。零代码、零轮询、零新 secret。
> Override:若 Neon 当前 plan 原生提供阈值推送,改为在 Neon 控制台配置(仍零代码)。

### 11.1 控制台入口路径

Neon Console(`https://console.neon.tech`)→ 选中本项目 → 左侧 **Usage / Billing**(用量与计费页)。按需也可进单个 branch / compute 的 **Monitoring** 看实时曲线。

### 11.2 关注指标清单

| 指标 | 看什么 | 为何关注 |
| --- | --- | --- |
| **Compute hours(计算时长)** | 本计费周期累计的 active compute 小时数 | scale-to-zero 下本应很低;异常飙升 = 有东西在持续唤醒 compute(可能是误加的主动探测,撞 NFR1)。 |
| **Storage(存储)** | 数据库占用容量 | `events` 表(T5)随时间增长,无清理(保留清理划 T7);盯住增速,逼近 plan 上限前要么清理要么升档。 |
| **Data transfer / 出网(egress)** | 出站流量 | 异常出网可能指向数据被大量拉取或循环查询。 |

### 11.3 红线数值(建议值,明博可调)

> 以下为**建议红线**,具体阈值明博按当时所选 Neon plan 的实际配额调整,不是硬契约。

- **Compute hours**:超过当月配额 **70%** 时排查是否有非预期的持续访问(scale-zero 下不该有);**90%** 时考虑限流或升档。
- **Storage**:超过 plan 容量 **80%** 时安排 T7 的保留清理或评估升档。
- **检查节奏建议**:与 GCP budget 告警(第 13 节)邮件触达同期手查一次,不设定时任务。

---

## 12. 主线外验证:蓝绿候选全流程(对应 T6 FR7 / FR8 / AC9 / D1)

> **G3 preview 决策(D1)**:复用现有蓝绿候选机制(`--no-traffic --tag candidate`)作为正式的「主线外验证」路径,
> **不建持久 preview 环境**(第二 Cloud Run service + Neon branch)。持久 preview 的四重新风险(孤儿 branch 计费 /
> 配置漂移 / 攻击面 / 坐标泄露)无现时收益,**defer 到客户端 `app-infra-toolkit` 联调需求出现时**再按真实形状 forge。

> **演练前提(对应 E4)**:本流程的回滚步骤需要**至少 2 个 revision**(候选 + 一个可回退的前序 revision)。
> 服务只有一个 revision 时无可回退目标,先正常部署积累一个前序 revision 再演练回滚。

本节把分散在第 4b / 5 / 6 / 7 节的命令整合成一条可照抄的闭环。

### 12.1 候选部署(不切流量)

```bash
gcloud run deploy server-infra-toolkit \
  --image <REGION>-docker.pkg.dev/<PROJECT_ID>/<AR_REPO>/<IMAGE>:$(git rev-parse --short HEAD) \
  --no-traffic --tag candidate \
  --service-account <PROJECT_NUMBER>-compute@developer.gserviceaccount.com \
  --allow-unauthenticated \
  --set-secrets NEON_DSN=NEON_DSN:latest \
  --max-instances=2 \
  --min-instances=0 \
  --region <REGION>
```

新 revision 部署但 0% 流量,Cloud Run 给它一个带 `candidate` 标签的独立 URL(见第 4b 节)。记下候选 URL。

### 12.2 候选 URL 冒烟

```bash
bash scripts/smoke.sh https://candidate---<service>.run.app   # 候选 URL(带 candidate 标签)
```

对候选 URL 跑第 5 节的 5 条断言,**全绿**才进切流量。任一红 → 不切流量,线上仍由旧 revision 服务,删候选或修复重部(无需回滚,因为流量从未切过去)。

### 12.3 切流量(冒烟全绿后)

```bash
gcloud run services update-traffic server-infra-toolkit --to-latest --region <REGION>
```

线上由候选 revision 接管 100% 流量。切完再对生产 URL 跑一次第 5 节冒烟做部署后确认。

### 12.4 回滚(切流量后发现问题)

```bash
# 1. 列出 revision,找到要回退的前一个 revision 名
gcloud run revisions list --service server-infra-toolkit --region <REGION>

# 2. 把流量显式切回前一 revision(100%)
gcloud run services update-traffic server-infra-toolkit --to-revisions=<prev-revision>=100 --region <REGION>
```

回滚后对生产 URL 跑冒烟,确认 `/livez` 返回的 `version` **等于前一 revision 的 SHA**(AC9)。

### 12.5 演练记录(T7 实操回写)

- **演练日期**:2026-06-10
- **候选 revision**:`server-infra-toolkit-00005-lew`(镜像 tag `40f57b2`)
- **回退目标 revision**:`server-infra-toolkit-00003-hzg`(version `56bc6eb`)
- **回滚后 `/livez` 的 `version`**:`56bc6eb`(`scripts/smoke.sh <prod-url> 56bc6eb` 断言通过)
- **全链路实走结果**:候选部署(0% 流量)→ 候选 URL 冒烟绿 → 切流(生产冒烟绿,version=`40f57b2`)→ 显式回滚(生产冒烟绿,version 退回 `56bc6eb`)→ 切回 LATEST 恢复(生产冒烟绿,version=`40f57b2`)。每步均以 `scripts/smoke.sh` 外部可观测断言背书,部署前置由 `scripts/deploy-precheck.sh` 四项全绿放行。

---

## 13. GCP Billing budget 告警(对应 T6 FR4 / AC5 / D3 / R2 / E2)

> **告警定位:事后知情层,不是实时防线。** GCP 账单数据自身延迟 24h+,budget alert 跨阈值的邮件触达**滞后**于真实消费(R2)。
> **实时防线是 `--max-instances=2`(部署时已设)+ T5 的请求体 / 条数硬上限**;budget alert 只负责"月度趋势跨阈值时知会一声"(D3)。

### 13.1 创建三档阈值 budget

`gcloud billing budgets create` 是 GA 命令组。三档阈值 = 重复传三次 `--threshold-rule`(percent 是 0.0–1.0 小数):

```bash
gcloud billing budgets create \
  --billing-account=<BILLING_ACCOUNT_ID> \
  --display-name="server-infra-toolkit-monthly" \
  --budget-amount=<MONTHLY_BUDGET>USD \
  --threshold-rule=percent=0.5 \
  --threshold-rule=percent=0.9 \
  --threshold-rule=percent=1.0
```

- `<BILLING_ACCOUNT_ID>` 是占位符,真实 billing account id 形态为 `XXXXXX-XXXXXX-XXXXXX`(只存本地 `.env`,NFR2);`<MONTHLY_BUDGET>` 替成月预算金额(如 `20`)。
- 三档 `percent=0.5 / 0.9 / 1.0` 即 50% / 90% / 100%;`basis` 默认 `current-spend`(已花费占比),需要按预测花费可加 `,basis=forecasted-spend`。

### 13.2 通知渠道说明(对应 E2)

- **默认通知对象**:budget 默认把告警邮件发给该 billing account 上所有 **Billing Account Administrator / Billing Account User** 角色的人。
- **⚠️ 默认收件人不一定是明博的常用邮箱(E2)。** 创建后**务必发一次测试通知确认明博常用邮箱能收到**(AC5):若默认渠道不对,改用自定义 Cloud Monitoring 通知渠道——
  ```bash
  # 在上面的 create 命令追加:挂自定义通知渠道(先在 Cloud Monitoring 建好 channel,拿到 CHANNEL_ID)
  --notifications-rule-monitoring-notification-channels=<CHANNEL_ID>
  ```
  或用 `--disable-default-iam-recipients` 关掉默认收件人后只走自定义渠道。
- **测试通知未达 → 修通知渠道后重验**,不接受"创建成功就算完"(E2)。
- **触达已验(2026-06-11,T6 实操)**:经临时 1HKD 测试预算实测,默认渠道告警邮件可达 billing admin 邮箱(条件满足后约 2 小时送达,符合评估周期预期;测试预算验后已删)。
- **⚠️ 口径坑(实测教训)**:budget 默认口径 `INCLUDE_ALL_CREDITS` 按**抵扣后净额**算——免费层把毛费用抵扣到 0 时,阈值永不跨线、邮件永不触发。**测试触达必须用毛额口径**:`--credit-types-treatment=exclude-all-credits` + 低阈值(如 `percent=0.01`)。正式预算保持默认净额口径(语义正确:真要掏钱才告警)。

---

## 14. 坐标 grep 自查(对应 T6 NFR2 / AC10 / E6)

> **目的**:T6 落地后全仓不得出现真实 GCP 坐标(project number / region / 服务域名 / billing account),文档一律占位符(NFR2)。
> 下面的 grep 模式**只匹配真实值形态**,占位符与测试 fixture 走白名单(E6:防占位符 / 示例误报)。

```bash
# A. project number(12 位数字),只在 GCP 语境出现时算命中,排除占位符 <PROJECT_NUMBER>。
#    含两支:数字在 GCP 关键字之后(projects/ / @ / projectNumber:),以及最常见的
#    泄露形态——compute SA 邮箱 `<12 位>-compute@`(数字在 @ 之前,前一支不命中)。
grep -rnE '(projects/|@|projectNumber["[:space:]:=]+)[0-9]{12}|[0-9]{12}-compute@' . \
  --include='*.md' --include='*.sh' --include='*.go' --include='*.yaml' --include='*.yml' --include='*.json' \
  | grep -v '\.git/' | grep -vE '<PROJECT_NUMBER>'

# B. 真实 region 字串(GCP region 形态:洲-方向+数字,如 <continent>-<dir><N>)
grep -rnE '\b(us|europe|asia|australia|northamerica|southamerica|me|africa)-[a-z]+[0-9]\b' . \
  --include='*.md' --include='*.sh' --include='*.go' --include='*.yaml' --include='*.yml' --include='*.json' \
  | grep -v '\.git/'

# C. run.app 真实服务域名,排除占位符 <...>.run.app 与示例域名
grep -rnE 'https?://[a-zA-Z0-9.-]+\.run\.app' . \
  --include='*.md' --include='*.sh' --include='*.go' --include='*.yaml' --include='*.yml' --include='*.json' \
  | grep -v '\.git/' | grep -vE '<[^>]+>\.run\.app|my-service-xyz\.run\.app'

# D. billing account(6-6-6 十六进制大写,如 XXXXXX-XXXXXX-XXXXXX 的真实值)
grep -rnE '\b[0-9A-F]{6}-[0-9A-F]{6}-[0-9A-F]{6}\b' . \
  --include='*.md' --include='*.sh' --include='*.go' --include='*.yaml' --include='*.yml' --include='*.json' \
  | grep -v '\.git/'
```

**白名单(已知非真实坐标,命中视为误报,见 E6)**:

- 占位符:`<REGION>` / `<PROJECT_ID>` / `<PROJECT_NUMBER>` / `<AR_REPO>` / `<IMAGE>` / `<BILLING_ACCOUNT_ID>` / `<MONTHLY_BUDGET>` / `<service>.run.app` / `<CHANNEL_ID>` / `<prev-revision>`。其中 compute SA 占位形态 `<PROJECT_NUMBER>-compute@developer.gserviceaccount.com` 含 `<PROJECT_NUMBER>` 子串,已被模式 A 的 `grep -vE '<PROJECT_NUMBER>'` 排除,不会误命中。
- 示例域名:`my-service-xyz.run.app`(`scripts/smoke.sh` 用法注释里的示意,非真实服务名)。
- 测试 fixture:auth 模块的 UUID fixture(`11111111-2222-4333-8444-555555555555` 等)内含的数字段——A 模式已用 GCP 语境锚定排除,不会误命中。

**判据**:四条命令(套用白名单 `grep -v` 后)**全部无输出**即通过(AC10)。

---
