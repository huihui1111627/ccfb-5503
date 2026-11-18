// Package sqlx 把引擎查询与结果对照到本机真实数据库（SQLite / PostgreSQL）。
//
// 为保持零外部依赖、避免 CGO 驱动，这里通过数据库自带的命令行客户端
// （sqlite3 / psql）以 CSV 模式交互：建表 -> 批量导入 -> 执行窗口查询 ->
// 解析 CSV 结果。所有交互都在临时目录中完成，不污染用户数据。
package sqlx

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dragonwindow/internal/table"
	"dragonwindow/internal/types"
)

// Dialect 标识目标数据库。
type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

// Result 是一条查询在真实数据库上的执行结果。
type Result struct {
	Columns []string
	Rows    [][]string // 全部按文本承载，比对时再归一化
	Err     string
	Elapsed time.Duration
}

// Runner 负责单个数据库的连接/建表/查询生命周期。
type Runner struct {
	dialect Dialect
	dir     string
	// postgres 专用
	dbName string
}

// NewSQLite 在临时目录创建一个 SQLite 数据库文件。
func NewSQLite() (*Runner, error) {
	dir, err := os.MkdirTemp("", "dw_sqlite_")
	if err != nil {
		return nil, err
	}
	return &Runner{dialect: SQLite, dir: dir, dbName: filepath.Join(dir, "test.db")}, nil
}

// NewPostgres 创建一个独立的临时数据库（前缀 dw_test_），退出时删除。
func NewPostgres() (*Runner, error) {
	dir, err := os.MkdirTemp("", "dw_pg_")
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("dw_test_%d", time.Now().UnixNano())
	if _, err := exec.LookPath("psql"); err != nil {
		return nil, fmt.Errorf("psql 客户端不可用: %w", err)
	}
	if out, err := exec.Command("createdb", name).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("createdb %s: %v (%s)", name, err, out)
	}
	return &Runner{dialect: Postgres, dir: dir, dbName: name}, nil
}

// Close 清理临时资源。
func (r *Runner) Close() error {
	if r.dialect == Postgres {
		_ = exec.Command("dropdb", "--if-exists", r.dbName).Run()
	}
	return os.RemoveAll(r.dir)
}

// DialectName 返回方言名。
func (r *Runner) DialectName() string { return string(r.dialect) }

// CreateTable 按表结构建表。
func (r *Runner) CreateTable(t *table.Table) error {
	var cols []string
	for _, c := range t.Columns {
		cols = append(cols, c.Name+" "+r.nativeType(c.Kind))
	}
	sql := fmt.Sprintf("CREATE TABLE %s (\n  %s\n);", t.Name, strings.Join(cols, ",\n  "))
	return r.execSQL(sql)
}

func (r *Runner) nativeType(kind string) string {
	if r.dialect == SQLite {
		switch kind {
		case types.TypeInteger:
			return "INTEGER"
		case types.TypeReal:
			return "REAL"
		case types.TypeBoolean:
			return "INTEGER" // SQLite 无 bool，用 0/1
		case types.TypeTimestamp:
			return "TEXT"
		case types.TypeInterval:
			return "TEXT"
		default:
			return "TEXT"
		}
	}
	// PostgreSQL
	switch kind {
	case types.TypeInteger:
		return "BIGINT"
	case types.TypeReal:
		return "DOUBLE PRECISION"
	case types.TypeBoolean:
		return "BOOLEAN"
	case types.TypeTimestamp:
		return "TIMESTAMPTZ"
	case types.TypeInterval:
		return "INTERVAL"
	default:
		return "TEXT"
	}
}

// LoadTable 把内存表数据写入数据库（走 CSV 批量导入）。
func (r *Runner) LoadTable(t *table.Table) error {
	csvPath := filepath.Join(r.dir, t.Name+".csv")
	if err := r.writeCSV(t, csvPath); err != nil {
		return err
	}
	switch r.dialect {
	case SQLite:
		// 写入无表头 CSV（writeCSVNoHeader），旧版 sqlite3 的 .import 直接按列导入。
		// 点命令通过 stdin 喂入，避免参数转义问题。
		dataPath := filepath.Join(r.dir, t.Name+"_data.csv")
		if err := r.writeCSVNoHeader(t, dataPath); err != nil {
			return err
		}
		script := fmt.Sprintf(".mode csv\n.import %s %s\n", dataPath, t.Name)
		// SQLite 的 .import 把空 CSV 字段导成空字符串而非 NULL。本数据集没有
		// 合法的空字符串值（文本列非空即 NULL），逐列把 '' 归一化为 NULL。
		for _, c := range t.Columns {
			script += fmt.Sprintf("UPDATE %s SET %s = NULL WHERE %s = '';\n", t.Name, c.Name, c.Name)
		}
		cmd := exec.Command("sqlite3", r.dbName)
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("sqlite import: %v (%s)", err, out)
		}
		return nil
	case Postgres:
		// \copy 从客户端文件导入，CSV 带表头。
		sql := fmt.Sprintf(`\copy %s FROM '%s' WITH (FORMAT csv, HEADER true, NULL '')`, t.Name, csvPath)
		cmd := exec.Command("psql", "-d", r.dbName, "-v", "ON_ERROR_STOP=1", "-c", sql)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("pg copy: %v (%s)", err, out)
		}
		return nil
	}
	return fmt.Errorf("unknown dialect")
}

// writeCSV 把表序列化成数据库可导入的 CSV。NULL 写空串；bool/time 按方言编码。
func (r *Runner) writeCSV(t *table.Table, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	header := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		header[i] = c.Name
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for row := 0; row < t.N; row++ {
		rec := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			rec[i] = r.encodeCell(c.Vals[row], c.Kind)
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// writeCSVNoHeader 仅写数据行（SQLite 旧版 .import 用），NULL 写空串。
func (r *Runner) writeCSVNoHeader(t *table.Table, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	for row := 0; row < t.N; row++ {
		rec := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			rec[i] = r.encodeCell(c.Vals[row], c.Kind)
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func (r *Runner) encodeCell(v any, kind string) string {
	if v == nil {
		return ""
	}
	if r.dialect == SQLite {
		switch x := v.(type) {
		case bool:
			if x {
				return "1"
			}
			return "0"
		case time.Time:
			return x.UTC().Format("2006-01-02 15:04:05.999999+00")
		case types.Interval:
			return types.FormatInterval(x)
		default:
			return types.FormatValue(v)
		}
	}
	// PostgreSQL：COPY CSV 直接用标准文本，bool/timestamp 都能解析。
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case time.Time:
		return x.UTC().Format("2006-01-02 15:04:05.999999+00")
	case types.Interval:
		return types.FormatInterval(x)
	default:
		return types.FormatValue(v)
	}
}

// execSQL 执行无结果集的语句。
func (r *Runner) execSQL(sql string) error {
	var cmd *exec.Cmd
	if r.dialect == SQLite {
		cmd = exec.Command("sqlite3", r.dbName, sql)
	} else {
		cmd = exec.Command("psql", "-d", r.dbName, "-v", "ON_ERROR_STOP=1", "-c", sql)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %v (%s)", r.dialect, err, stderr.String())
	}
	return nil
}

// QueryCSV 执行查询并以 CSV（含表头）返回解析后的结果。
func (r *Runner) QueryCSV(sql string) (*Result, error) {
	t0 := time.Now()
	var cmd *exec.Cmd
	if r.dialect == SQLite {
		// -header -csv 让 sqlite3 直接输出带表头 CSV。
		cmd = exec.Command("sqlite3", "-header", "-csv", r.dbName, sql)
	} else {
		// psql 用 COPY (...) TO STDOUT CSV HEADER 输出纯 CSV，避免边框噪声。
		wrapped := fmt.Sprintf("COPY (%s) TO STDOUT WITH (FORMAT csv, HEADER true)", sql)
		cmd = exec.Command("psql", "-d", r.dbName, "-v", "ON_ERROR_STOP=1", "-c", wrapped)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := &Result{Elapsed: time.Since(t0)}
	if err != nil {
		res.Err = strings.TrimSpace(stderr.String())
		return res, nil // 错误作为结果返回，由上层判断兼容性
	}
	parsed, perr := csv.NewReader(bytes.NewReader(stdout.Bytes())).ReadAll()
	if perr != nil {
		return nil, fmt.Errorf("parse %s csv: %w", r.dialect, perr)
	}
	if len(parsed) == 0 {
		return res, nil
	}
	res.Columns = parsed[0]
	res.Rows = parsed[1:]
	return res, nil
}

// ParseInt 是 CSV 字段转整数的助手。
func ParseInt(s string) (int64, error) { return strconv.ParseInt(strings.TrimSpace(s), 10, 64) }
