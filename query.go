package kpirank

import (
	"fmt"
	"math/big"
	"os"
	"sort"
)

// Query 执行分组排行，返回按规则排序后的前 q.Limit 个分组。
// 分组不足时全部返回。空平均值的分组排在非空之后。
func (d *Dataset) Query(q Query) ([]GroupResult, error) {
	if len(q.Dimensions) < 1 || len(q.Dimensions) > 3 {
		return nil, fmt.Errorf("维度数量须为 1 至 3，实际 %d", len(q.Dimensions))
	}
	if q.Limit < 1 || q.Limit > 10000 {
		return nil, fmt.Errorf("返回数量须为 1 至 10000，实际 %d", q.Limit)
	}
	colIdx := make(map[string]int, len(d.columns))
	for i, name := range d.columns {
		colIdx[name] = i
	}
	// queryCols: 查询维度顺序的列下标
	queryCols := make([]int, len(q.Dimensions))
	seen := make(map[string]bool, len(q.Dimensions))
	for i, dim := range q.Dimensions {
		idx, ok := colIdx[dim]
		if !ok {
			return nil, fmt.Errorf("维度列不存在: %q", dim)
		}
		if idx == d.metricCol {
			return nil, fmt.Errorf("指标列不能作为维度: %q", dim)
		}
		if seen[dim] {
			return nil, fmt.Errorf("维度列重复: %q", dim)
		}
		seen[dim] = true
		queryCols[i] = idx
	}
	// wantCol: 升序列下标（派生文件按列序存储）；perm: 升序序 -> 查询序
	wantCol := make([]int, len(queryCols))
	copy(wantCol, queryCols)
	sort.Ints(wantCol)
	perm := make([]int, len(wantCol)) // perm[查询序位置] = 升序序位置
	for qi, c := range queryCols {
		for si, wc := range wantCol {
			if wc == c {
				perm[qi] = si
				break
			}
		}
	}

	dr, err := openDerived(d.dataFile)
	if err != nil {
		return nil, err
	}
	defer dr.close()

	tmpDir, err := os.MkdirTemp("", "kpirank-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	buckets, err := partition(dr, wantCol, tmpDir)
	if err != nil {
		return nil, err
	}

	tk := newTopK(q.Limit)
	for _, path := range buckets {
		err := aggregateBucket(path, func(enc string, g *groupAgg) {
			res := &GroupResult{
				Key:   make([]string, len(g.items)),
				Count: g.count,
				Rows:  g.rows,
				Sum:   g.sum,
			}
			for qi := range res.Key {
				res.Key[qi] = g.items[perm[qi]]
			}
			if g.count > 0 {
				res.Avg = new(big.Rat).SetFrac(g.sum, big.NewInt(g.count))
			}
			tk.offer(res)
		})
		if err != nil {
			return nil, err
		}
	}
	return tk.sorted(), nil
}
