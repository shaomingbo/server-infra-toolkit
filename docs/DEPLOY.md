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

> **状态:DEFERRED(尚未执行,线上未验)。** 双缩零重试(`internal/platform/db/retry.go`)目前只有本地单测覆盖
> 重试逻辑(`isTransientConnError` / budget 绑定)。「真实 Cloud Run + Neon 都缩零后首请求经重试返回成功」属
> **线上验收**,依赖 T0 部署链(第 1–7 节)就绪后才能跑。**禁止用本地 / 单测绿冒充线上绿**——单测验的是
> 重试代码路径,不能证明 Neon 冷启动的真实时延与 SQLSTATE 落在重试预算 / 白名单内(retry.go 注释里那些
> UNCONFIRMED 的冷启动时延 / SQLSTATE 就是靠这次验收来证实并回调参数的)。

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
