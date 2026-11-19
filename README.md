# 龙舟赛窗口函数计算引擎

用 Go 实现的窗口函数计算引擎：输入是一张表，输出是追加了窗口计算列的表，附带一个网页工作台。

针对龙舟赛成绩系统（每场要出排名、分段用时、与前一名的差距、滑动平均桨频）。SQL 里复杂窗口跑几分钟的痛点，由「一次排序 + 线段树聚合」解决。

## 快速开始

```bash
# 网页工作台（数据表 / 窗口定义 / 执行计划 / 结果与耗时）
go run ./cmd/wfserver -addr 127.0.0.1:8080

# 跑一条查询，带执行计划和参考实现比对
go run ./cmd/wfquery -rows 20000 -ref

# 性能基准
go run ./cmd/wfbench -rows 300000 -ref

# 完整验证（引擎 vs 逐行暴力参考实现、vs PostgreSQL、vs SQLite）
go run ./cmd/wfverify -rows 400000 -queries 200
```

## 功能

查询语言是 PostgreSQL 窗口语法的子集（`SELECT ... FROM races [WINDOW ...] [ORDER BY ...]`），
窗口函数可以出现在选择列表中，普通列会原样透传。

### 1. 窗口帧：ROWS / RANGE / GROUPS

- `ROWS`：按行数偏移。
- `RANGE`：按排序键的值距离偏移；数值列配数值偏移，时间/日期列配 `INTERVAL`。
- `GROUPS`：按 peer 组（排序键相同的一组行）偏移。

每种单位的边界都支持 `UNBOUNDED PRECEDING`、`n PRECEDING`、`CURRENT ROW`、`n FOLLOWING`、`UNBOUNDED FOLLOWING`，
并执行 PostgreSQL 的合法性检查（例如 `CURRENT ROW` 之后不能接 `PRECEDING`、帧尾不能是 `UNBOUNDED PRECEDING` 等）。

### 2. 帧排除 EXCLUDE

`EXCLUDE NO OTHERS`（默认）、`EXCLUDE CURRENT ROW`、`EXCLUDE GROUP`（去掉当前行的整个 peer 组）、
`EXCLUDE TIES`（去掉同组其它行、保留当前行）。`MIN`/`MAX` 在排除后需要重算极值，引擎会按需重扫该帧。

### 3. 线段树聚合

帧滑动时，`COUNT/SUM/AVG/MIN/MAX` 通过每个分区一棵线段树在 **O(log n)** 内求出区间结果，
不会对每一行重新扫描帧。多个窗口项如果帧相同，共享同一份帧边界计算结果。

### 4. 偏移取值函数

`lag/lead(value [, offset [, default]])`：

- offset 可以是任意整数（负数翻转方向，与 PostgreSQL 一致），也可以是**每一行各自的表达式**；
- 越界（或目标为 NULL）时返回 default，default 本身也是逐行求值的表达式；
- `IGNORE NULLS` 扩展：跳过空值，取前/后第 n 个非空值（PostgreSQL 16 不支持该语法，这里按 SQL:2016 语义实现）。

`nth_value(value, n)` 同样支持 `IGNORE NULLS`。

### 5. 每行偏移表达式（扩展）

帧边界偏移可以引用当前行：`ROWS BETWEEN v % 3 PRECEDING AND 1 FOLLOWING`。
PostgreSQL 16 会报 `argument of ROWS must not contain variables`；引擎按「在每一行上求值」实现。
常量偏移只求值一次。

### 6. 多窗口共享排序

执行前对全部窗口按 `(PARTITION BY, ORDER BY)` 分组，相同的组合只排序一次（执行计划里可以看到
`S1`、`S2` … 以及每个排序承载的窗口项）。分组、排序键与 ORDER BY 表达式的求值都只做一次。

### 7. RANGE 的空值处理

排序键中的 NULL 按 `NULLS FIRST` / `NULLS LAST` 放在两端。RANGE 偏移与空值的交互完全对齐
PostgreSQL 16（见下）：非空当前行加有限偏移时，NULL 段被视为 ±∞，不会进入帧；
`UNBOUNDED` 与 `CURRENT ROW` 则包含 NULL 段；当前行为 NULL 时，有限偏移的边界落在 NULL 段内。

### 8. 与 PostgreSQL 对齐

- 默认帧：有 ORDER BY 时是 `RANGE UNBOUNDED PRECEDING .. CURRENT ROW`，否则是整个分区。
- ASC 默认 `NULLS LAST`，DESC 默认 `NULLS FIRST`。
- `rank/dense_rank/percent_rank/cume_dist/ntile/row_number` 的 peer 定义与边界值。
- `avg(int)` 返回 numeric、`avg(float)` 返回 double，`sum(int)` 不溢出（必要时提级）。
- 帧校验、偏移必须非负/非空、RANGE 偏移类型兼容性等错误规则。
- peer 组内的行序在 SQL 中未定义；引擎按输入顺序稳定排序。

## 网页工作台

`go run ./cmd/wfserver` 后打开 `http://127.0.0.1:8080`，页面分四块：

1. **数据表**：当前赛事数据（可翻页、可重新生成），展示列名与类型。
2. **窗口定义与查询**：SQL 编辑区；下面是随机生成的赛事查询，点击即可填入。
3. **执行计划**：每次排序承载了哪些窗口项、每个窗口项的帧、排除、聚合策略（线段树 / peer 扫描 / 顺序统计）。
4. **结果与耗时**：结果表、行数、引擎耗时、吞吐，以及（勾选时）与逐行暴力参考实现的比对结果和加速比。

`⌘/Ctrl + Enter` 执行查询。「抽样验证参考实现」按钮会对当前数据跑一批随机查询并逐行比对。

## 验证

正确性由三层验证保证：

1. **逐行暴力参考实现**（`internal/ref`）：对每一行重新扫描帧、逐项聚合。它与引擎共享表达式与类型层，
   但窗口算法完全独立。`go test ./internal/window/` 用几千条随机查询（含 peer、NULL、三种帧单位、
   三种排除、每行偏移、IGNORE NULLS）逐行比对。
2. **PostgreSQL 16**（`internal/dbcompare`）：本地服务器可用时，同一批查询在 PG 中执行并比对。
   随机语料会被筛掉 PostgreSQL 不支持的扩展语法；并列导致的 ROWS 帧内行序差异属于 SQL 未定义行为，
   对照时用唯一排序键的数据保证可比较。
3. **SQLite**：同一套查询在 SQLite 中执行（SQLite 的空值默认位置与 PostgreSQL 相反，因此显式写 NULLS）。

`go run ./cmd/wfverify` 一把跑完三层。

## 代码结构

```
cmd/wfserver     网页服务
cmd/wfquery      单条查询：计划 + 结果 + 可选比对
cmd/wfbench      性能基准
cmd/wfverify     完整验证套件
internal/value   值模型、比较、区间算术
internal/table   表模型
internal/expr    表达式解析与求值
internal/window  窗口解析（SQL 子集）、排序/分区、帧计算、线段树、各窗口函数
internal/ref     逐行暴力参考实现
internal/gen     赛事数据与随机查询生成
internal/dbcompare PostgreSQL / SQLite 对照
internal/web     页面与 JSON API
```

## 性能

400,000 行赛事数据、14 列（含空值、时间戳、浮点、字符串），单次查询耗时（本机 14 核）：

| 查询 | 耗时 | 吞吐 |
|---|---|---|
| 分区累计和 | ~0.65 s | 0.62M 行/s |
| 滑动 SUM 100 行 | ~0.56 s | 0.71M 行/s |
| 滑动 AVG 1000 行 | ~0.65 s | 0.61M 行/s |
| RANGE 2 分钟 | ~0.77 s | 0.52M 行/s |
| GROUPS 2 组 | ~0.62 s | 0.64M 行/s |
| 三个窗口共享一次排序 | ~0.47 s | 0.85M 行/s |
| 三个不同排序 | ~1.1 s | 0.37M 行/s |

相同数据上相对逐行暴力参考实现，滑动帧聚合有数十到上百倍加速（帧越大差距越大），
因为参考实现每行 O(帧宽)，引擎每行 O(log n)。
