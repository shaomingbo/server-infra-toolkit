# 迁移规范(server-infra-toolkit / T1 数据接入层)

本目录是数据库 schema 的**单一真相源**(NFR6):所有 schema 演进都以这里累积的
迁移文件为准,sqlc(T2)从这里读 schema 生成 query。迁移工具为 **goose v3**
(版本锁在独立的 `tools` module:`tools/go.mod` 的 `tool` 指令,不污染主服务
module;在 `tools` 目录内用 `go tool goose` 调用,不全局安装)。

> 选型背景:goose v3 经异源对比选定(明博拍板)。迁移端点用 Neon 直连
> (`NEON_DSN` 已是直连串)。迁移**不在服务启动路径执行**(NG7),是部署前的独立一步。

## 目录布局与命名

- 迁移文件全部放在本目录,单文件同时含 Up / Down 两段(goose 单文件格式)。
- 文件名**序号补零**:`00001_xxx.sql`、`00002_xxx.sql` …… 五位零填充。
  - 原因:sqlc 按字典序读迁移文件,goose `-s`(sequential)也用序号定序。
    用 `1` / `2` / `10` 会在字典序下乱序(`10` 排在 `2` 前),必须补零。
- 文件内格式:
  ```sql
  -- +goose Up
  <建/改 schema 的 DDL + 必要种子>

  -- +goose Down
  <撤销上面变更的 DDL>
  ```
  - 含分号的复杂语句(如函数体、`DO $$ ... $$`)用 `-- +goose StatementBegin`
    / `-- +goose StatementEnd` 包起来,否则 goose 会按分号错误切分。

## 工作流顺序

1. 在本目录加迁移文件(先建 schema)。
2. `cd tools && go tool sqlc -f ../sqlc.yaml generate`(sqlc 从迁移文件读 schema 生成 query;
   sqlc.yaml 里的 schema/queries/out 相对路径以 sqlc.yaml 所在目录(仓库根)为基准,
   不受 CWD 在 `tools` 影响)。
3. 本地编译 + CI gate 校验。

**顺序不能反**(E9):先写引用某表的 sqlc query、表却还没迁移建出来 → generate 失败。

## 调用方式(本任务只建文件,实跑见下方)

goose 在独立的 `tools` module 内调用,故所有命令先 `cd tools`;`-dir` 指向迁移目录
用相对 `tools` 的路径 `../db/migrations`。

```sh
# 版本 / 状态(注意:并非绝对只读 —— 见下方说明)
cd tools
go tool goose -dir ../db/migrations postgres "$NEON_DSN" version
go tool goose -dir ../db/migrations postgres "$NEON_DSN" status

# 前进 / 回退
go tool goose -dir ../db/migrations postgres "$NEON_DSN" up        # 迁到最新
go tool goose -dir ../db/migrations postgres "$NEON_DSN" up-by-one # 只前进 1 步
go tool goose -dir ../db/migrations postgres "$NEON_DSN" down      # 回退 1 步(读下方边界!)
```

> **`version` / `status` 不是绝对只读**:这两个子命令底层都会调 goose 的
> `EnsureDBVersionContext`(见 goose v3 `status.go`),若版本表 `goose_db_version`
> 还不存在(空库首次跑),它会先 **建出版本表** 再读。所以在全新库上首次执行
> `version` / `status` 会写库(创建版本表),并非纯查询。真正只读的是直接连库手写的
> SQL 查询(`information_schema` / `\d` 之类),那才不改任何状态。

goose 用版本表 `goose_db_version` 记录已应用的迁移版本(本 repo 不改默认表名)。
该表是 goose 内部状态,**不是** schema 真相源、sqlc 读不到——所以自验载体是迁移
文件里显式 CREATE 的 `_infra_selftest` 表,而非复用版本表(见 `00001_infra_selftest.sql` 头注)。

---

## AC7 四点规范

### ① 每个迁移在事务内执行(goose 默认)

goose 默认把单个迁移的整个 Up(或整个 Down)包在**一个事务**里执行。某条 DDL
半途失败 → 整个迁移原子回滚到执行前,不会留下「半应用」的 schema(FR6 / E3)。
Postgres 支持事务 DDL,这条才成立。

**例外:不可事务化的 DDL**(如 `CREATE INDEX CONCURRENTLY`、某些 `ALTER TYPE ... ADD VALUE`)
不能放在事务里。这类迁移在文件**最顶部**显式标注:

```sql
-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY ...
```

`NO TRANSACTION` 对整个文件生效(Up 和 Down 都不进事务)。代价:这类迁移半途失败
**不会自动回滚**,可能让库 schema 半应用、与版本表记录不一致,需按 ② 手工恢复——
所以仅在确有必要时使用。

### ② goose 失败 /「库 schema 与版本表记录不一致」恢复步骤

> 措辞说明:goose v3 **没有** golang-migrate 那种 `dirty` 标志位。其版本表
> `goose_db_version` 只有 `version_id` / `is_applied` / `tstamp` 三个业务列(外加一个
> `id` 自增主键),没有任何 dirty 字段(已对 goose v3.27.1 postgres dialect 源码核实)。
> 所以这里说的「卡住 / 脏状态」指的是 **库实际 schema 与版本表记录不一致**(例如版本表
> 记到 v2、但库里 v2 的表只建了一半),而非某个 dirty 布尔位。

事务内执行的迁移半途失败,Postgres 会回滚该迁移的事务,通常版本表干净、直接修
SQL 重跑即可。会出现「卡住 / 库 schema 与版本表对不上」的主要是 `NO TRANSACTION` 迁移
半途失败,或迁移文件已落库后又被改动。恢复步骤(命令均在 `tools` 目录内执行):

1. `cd tools && go tool goose -dir ../db/migrations postgres "$NEON_DSN" status` ——看哪些
   版本已应用、当前停在哪;`... version` 看当前版本号。
2. **判断库的真实 schema 与版本表是否一致**:连库实际查表/索引是否存在
   (`\d` / `information_schema`),与版本表记录的「已到版本」对照。
3. **手工把库恢复到某个版本对应的干净 schema**:
   - 若某迁移只部分应用(NO TRANSACTION 场景),手工执行其 Down 段剩余部分,或手工
     补齐 Up 段剩余部分,使库 schema 与某个完整版本一致。
   - 必要时手工修正版本表 `goose_db_version` 里的记录,使其与库实际状态一致
     (这是最后手段,改前先确认实际 schema)。
4. 修正迁移文件的 SQL(根因),`cd tools && go tool goose -dir ../db/migrations ... up`
   重跑;`... status` 复核到位。

经验法则:**优先把库 schema 改到与某个干净版本一致,再让版本表对齐**,不要反过来
只改版本表而把库留在半应用状态。能事务化的迁移就别用 NO TRANSACTION,从源头避免
「库 schema 与版本表不一致」。

### ③ down 适用边界(本地/未发布撤销 vs 生产前滚修复)

- **本地 / 未发布的迁移**:`down` / `down-to` / `redo` 自由用——撤销刚写还没发出去的
  迁移,迭代调试。
- **生产环境**:**默认不跑 down**。已发布的 schema 出问题,默认走**前滚修复**——写一条
  新迁移(`00002_...`)把 schema 改对,而不是回退旧迁移。
  - 原因:down 可能丢数据(见 ④);多实例/已有数据下回退旧迁移风险高于前滚。
  - 牢记「代码回滚 ≠ schema 回滚」(NFR9):`git revert` T1 代码**不会**撤销已在 Neon
    跑过的迁移,schema 与代码会脱节,需独立处理(默认前滚)。

### ④ 破坏性 down(DROP)标 IRREVERSIBLE 且不入常规回滚流程

凡 Down 段含 `DROP TABLE` / `DROP COLUMN` 等**破坏性**操作(语法上「可回滚」但数据
不可逆丢失,E4),必须:

- 在该 Down 段**显式标注** `IRREVERSIBLE` 注释,说明会丢什么数据。
- **不纳入常规生产回滚流程**:生产遇问题走 ③ 的前滚修复,不跑这类 down。
- 仅允许在本地 / 未发布、且明确接受数据丢失时执行。

示例见本目录 `00001_infra_selftest.sql` 的 Down 段:它是 `DROP TABLE`,已标
IRREVERSIBLE;因自验表无业务数据,本地/CI 跑 down 验证安全,但仍按规范不入生产回滚。
