// Package kpirank 提供离线 KPI 分组排行能力：从本地 CSV 载入数据，
// 在载入完成后由调用方选择 1 至 3 个维度执行分组排行。
package kpirank

import "math/big"

// Dataset 表示一次载入完成的数据集，派生数据存放在本地文件中。
type Dataset struct {
	dir        string   // 派生文件所在目录
	dataFile   string   // 派生数据文件路径
	columns    []string // 全部列名（含指标列）
	metricCol  int      // 指标列下标
	metricName string   // 指标列名
	rows       int64    // 记录数
}

// GroupResult 是一个分组的排行结果。
type GroupResult struct {
	Key   []string // 维度值元组，长度与查询维度数一致
	Count int64    // 有效计数（指标非空的记录数）
	Sum   *big.Int // 有效指标总和；全部为空时为 0
	Avg   *big.Rat // 平均值；全部指标为空时为 nil
	Rows  int64    // 该组的总记录数（含指标为空的记录）
}

// Query 描述一次分组排行查询。
type Query struct {
	Dimensions []string // 1 至 3 个维度列名
	Limit      int      // 返回数量，1 至 10000
}
