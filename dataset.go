// Package kpirank 提供离线 KPI 分组排行能力：
// 从本地 CSV 载入数据到本地文件存储，并在载入后按调用方
// 指定的 1~3 个维度做分组排行（按平均指标降序）。
package kpirank

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
)

// 业务约束常量。
const (
	MaxRows      = 500000 // 单份数据最大行数
	MaxColumns   = 100    // 最大列数
	MaxCellBytes = 256    // 单个单元格最大 UTF-8 字节数
	MinTopN      = 1      // 返回数量下限
	MaxTopN      = 10000  // 返回数量上限
)

// 公共错误。
var (
	ErrClosed          = errors.New("kpirank: dataset closed")
	ErrMetricColumn    = errors.New("kpirank: invalid metric column")
	ErrDimensionColumn = errors.New("kpirank: invalid dimension column")
	ErrDimensionCount  = errors.New("kpirank: dimensions must be 1 to 3")
	ErrTopNRange       = errors.New("kpirank: topN must be 1 to 10000")
	ErrTooManyRows     = errors.New("kpirank: too many rows")
	ErrTooManyColumns  = errors.New("kpirank: too many columns")
	ErrCellTooLarge    = errors.New("kpirank: cell exceeds 256 bytes")
	ErrDuplicateColumn = errors.New("kpirank: duplicate column name")
)

// metaFileName 是派生元数据文件名。
const metaFileName = "meta.json"

// GroupResult 是一个分组的排行结果。
type GroupResult struct {
	Key   []string // 维度值元组，与查询维度顺序一致
	Count int64    // 有效（非空）指标计数
	Sum   *big.Int // 有效指标总和（可能超出 int64）
	Avg   *big.Rat // 平均值；该组全部指标为空时为 nil
}

// Dataset 是已载入的数据集句柄，持有本地文件存储。
type Dataset struct {
	dir       string   // 数据文件所在目录
	columns   []string // 全部列名（表头顺序）
	metricIdx int      // 指标列下标
	nRows     int64    // 记录数
	store     *rowStore
	closed    bool
}

// meta 是持久化到本地文件的派生元数据。
type meta struct {
	Columns   []string `json:"columns"`
	MetricIdx int      `json:"metric_idx"`
	NumRows   int64    `json:"num_rows"`
}

// Columns 返回全部列名（表头顺序）。
func (d *Dataset) Columns() []string {
	out := make([]string, len(d.columns))
	copy(out, d.columns)
	return out
}

// NumRows 返回记录数。
func (d *Dataset) NumRows() int64 { return d.nRows }

// LoadCSV 载入本地 CSV 文件，metricColumn 为指标列名。
// 原始数据与派生数据写入 dir 下的本地文件。
func LoadCSV(dir, csvPath, metricColumn string) (*Dataset, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.ReuseRecord = true

	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("kpirank: read header: %w", err)
	}
	if len(header) > MaxColumns {
		return nil, ErrTooManyColumns
	}
	seen := make(map[string]int, len(header))
	metricIdx := -1
	for i, name := range header {
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateColumn, name)
		}
		seen[name] = i
		if name == metricColumn {
			metricIdx = i
		}
	}
	if metricIdx < 0 {
		return nil, fmt.Errorf("%w: %q", ErrMetricColumn, metricColumn)
	}

	store, err := openRowStore(dir, len(header))
	if err != nil {
		return nil, err
	}
	d := &Dataset{
		dir:       dir,
		columns:   append([]string(nil), header...),
		metricIdx: metricIdx,
		store:     store,
	}
	fail := func(err error) (*Dataset, error) {
		store.remove()
		return nil, err
	}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("kpirank: read csv: %w", err))
		}
		for _, c := range rec {
			if len(c) > MaxCellBytes {
				return fail(ErrCellTooLarge)
			}
		}
		if err := store.appendRow(rec); err != nil {
			return fail(err)
		}
		if store.nRows > MaxRows {
			return fail(ErrTooManyRows)
		}
	}
	d.nRows = store.nRows
	if err := store.close(); err != nil {
		return fail(err)
	}
	if err := d.writeMeta(); err != nil {
		return fail(err)
	}
	return d, nil
}

// writeMeta 将派生元数据写入本地文件。
func (d *Dataset) writeMeta() error {
	m := meta{Columns: d.columns, MetricIdx: d.metricIdx, NumRows: d.nRows}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d.dir, metaFileName), data, 0o644)
}

// GroupRank 按 dims（1~3 个维度列名）分组，按平均值降序返回前 topN 组。
// 平均值精确相等时按维度元组逐项 UTF-8 字节序升序；
// 空平均值排在所有非空平均值之后。
func (d *Dataset) GroupRank(dims []string, topN int) ([]GroupResult, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if len(dims) < 1 || len(dims) > 3 {
		return nil, ErrDimensionCount
	}
	if topN < MinTopN || topN > MaxTopN {
		return nil, ErrTopNRange
	}
	dimIdx := make([]int, len(dims))
	for i, name := range dims {
		idx := -1
		for j, c := range d.columns {
			if c == name {
				idx = j
				break
			}
		}
		if idx < 0 || idx == d.metricIdx {
			return nil, fmt.Errorf("%w: %q", ErrDimensionColumn, name)
		}
		dimIdx[i] = idx
	}
	return d.groupRank(dimIdx, topN)
}

// Close 关闭数据集并保留磁盘文件。
func (d *Dataset) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	return d.store.close()
}

// Cleanup 关闭数据集并删除磁盘文件。
func (d *Dataset) Cleanup() error {
	if d.closed {
		return nil
	}
	d.closed = true
	if err := d.store.remove(); err != nil {
		return err
	}
	return os.Remove(filepath.Join(d.dir, metaFileName))
}
