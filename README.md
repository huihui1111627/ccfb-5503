# dragonwindow — Go 窗口函数计算引擎

为龙舟赛事成绩分析实现的窗口函数引擎：输入内存表与窗口查询 SQL，输出带窗口
计算结果的表。核心用**泛型线段树**把帧聚合从逐行扫描的 O(帧长) 降到每行
O(log n)，语义向 PostgreSQL 对齐。

## 目录结构

```
cmd/dragonwindow/   CLI 入口（gen / querygen / verify / crosscheck / bench / serve）
internal/
  types/            标量值类型（NULL、整数、浮点、布尔、文本、时间戳、间隔）
  table/            列式内存表 + JSON 编解码
  expr/             SQL 词法/语法分析、表达式求值、窗口规格绑定与校验
  window/segtree/   泛型幺半群线段树
  window/           引擎主体：排序、帧边界、EXCLUDE、聚合、排名、导航、投影
  ref/              逐行暴力扫描参考实现（独立代码路径，差分测试基线）
  datagen/          龙舟赛事模拟数据生成
  querygen/         随机窗口查询生成（覆盖全部特性组合）
  sqlx/             SQLite / PostgreSQL 跨库对照（走 CLI，零 CGO 依赖）
  web/              查询页面与 HTTP API（go:embed 打包静态资源）
```

## 已实现的窗口能力

- **帧单位**：`ROWS` / `RANGE` / `GROUPS`，边界支持无界、当前行、偏移量。
- **帧偏移**：常量或**每行各自求值的表达式**（动态偏移）。
- **RANGE**：数值偏移与 `INTERVAL` 时间间隔偏移；排序键 NULL 的前置/后置。
- **EXCLUDE**：`CURRENT ROW` / `GROUP` / `TIES` / `NO OTHERS`。
- **聚合**：`count`（含 `count(*)` 与 `count(列)` 的 NULL 语义）、`sum`、`avg`、
  `min`、`max`、`stddev_samp/pop`、`var_samp/pop`、`string_agg`，支持 `FILTER`。
- **排名**：`row_number`、`rank`、`dense_rank`、`percent_rank`、`cume_dist`、`ntile`。
- **导航**：`lag` / `lead`（偏移、默认值、`IGNORE NULLS` 跳过空值）、
  `first_value` / `last_value` / `nth_value`（受帧与 EXCLUDE 约束）。
- 具名窗口 `WINDOW` 子句、共享排序（同规格窗口只排序一次）、`WHERE`、输出
  `ORDER BY`、`LIMIT`、CASE/标量表达式与窗口结果的嵌套运算。

## 关键设计

### 线段树 + 幺半群

每个聚合建模为满足结合律的幺半群（单位元 + Combine），在每个分区上建树。
帧在 EXCLUDE 后被拆成至多三个有序子区间，分别 O(log n) 查询后按从左到右
顺序合并，结果与逐行左折叠一致。

浮点求和与方差用 `big.Rat` 保存精确中间状态，避免浮点结合顺序差异，使引擎
与暴力参考实现能逐值严格一致；最终结果再转回 float64。

### 共享排序

窗口按「分区表达式 + 排序表达式」的规范化文本生成指纹，指纹相同的多个窗口
复用同一个 `SortedPerm`，一次排序服务多条窗口函数。

### 性能优化

- **累计帧快速路径**：`UNBOUNDED PRECEDING ... CURRENT ROW`（无 EXCLUDE）是累计
  排名/累计用时的常见形态，识别后走线性前缀计算，绕开每行 O(log n) 的树查询；
  ROWS 逐点推进、RANGE/GROUPS 按 peer 组推进，结果与树路径严格一致。
- **string_agg 前缀缓冲**：所有有效字符串写入共享 builder 并记录前缀，累计帧
  O(1) 取用，消除逐行重拼整帧的 O(n²) 拷贝。
- **无 WHERE 零拷贝**：无过滤条件时直接复用输入表，不做全表复制。

实测（Apple M4 Pro，30 万行、300 条随机窗口查询）引擎全部跑通，平均约
210ms/查询；同规模下相对逐行暴力扫描有数量级的加速（中等规模基准约 14x）。

### 差分测试

`ref` 包是与引擎刻意独立的暴力实现：帧边界按 SQL 定义逐行判定、聚合对帧成员
逐行左折叠，不使用线段树。`verify` 命令对几十万行随机数据、几百条随机窗口
查询逐值比对两套实现。

## 使用

```bash
# 生成赛事数据
go run ./cmd/dragonwindow gen -rows 300000

# 生成随机窗口查询
go run ./cmd/dragonwindow querygen -n 300

# 引擎 vs 暴力参考，逐值差分验证
go run ./cmd/dragonwindow verify -rows 300000 -queries 300

# 与本机 SQLite / PostgreSQL 对照
go run ./cmd/dragonwindow crosscheck -rows 20000 -queries 150 -db both

# 性能基准（线段树引擎 vs 暴力扫描）
go run ./cmd/dragonwindow bench -rows 100000 -queries 40

# 启动查询页面（数据表 / 窗口定义 / 执行计划 / 结果 / 耗时 五个区块）
go run ./cmd/dragonwindow serve -addr :8080
```

## 测试

```bash
go test ./...
```

## 与真实数据库的差异说明

- 跨库对照时会给窗口 `ORDER BY` 追加唯一决胜列 `row_id`，强制并列行顺序确定，
  使 `row_number`/`lag`/`nth_value` 等对并列顺序敏感的函数可逐行比对。
- 带表达式的动态帧偏移、`IGNORE NULLS`、`GROUPS`、`EXCLUDE` 等是 SQLite/
  PostgreSQL 16 不支持或语法不同的能力，仅在引擎与暴力参考间验证，跨库对照跳过。
