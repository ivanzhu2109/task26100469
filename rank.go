package kpirank

import (
	"container/heap"
	"math/big"
	"strings"
)

// compareGroup 定义排行的全序：
//  1. 非空平均值排在空平均值之前；
//  2. 平均值降序（big.Rat 精确比较，无舍入）；
//  3. 平均值相等（或均为空）时，按维度元组逐项 UTF-8 字节序升序。
//
// 返回负数表示 a 排在 b 之前。
func compareGroup(aItems []string, aAvg *big.Rat, bItems []string, bAvg *big.Rat) int {
	if aAvg == nil && bAvg == nil {
		return compareItems(aItems, bItems)
	}
	if aAvg == nil {
		return 1
	}
	if bAvg == nil {
		return -1
	}
	if c := aAvg.Cmp(bAvg); c != 0 {
		return -c // 降序
	}
	return compareItems(aItems, bItems)
}

func compareItems(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := strings.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

// resultHeap 是最坏元素在堆顶的最大堆（按排行顺序），容量受限，
// 用于流式维护前 K 名。
type resultHeap struct {
	items []*GroupResult
}

func (h resultHeap) Len() int { return len(h.items) }

// Less 反向：排行更差的元素视为“更小”，从而沉到堆顶被优先淘汰。
func (h resultHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	return compareGroup(a.Key, a.Avg, b.Key, b.Avg) > 0
}

func (h resultHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *resultHeap) Push(x any) { h.items = append(h.items, x.(*GroupResult)) }

func (h *resultHeap) Pop() any {
	old := h.items
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	h.items = old[:n-1]
	return it
}

// topK 维护容量为 k 的堆，offer 一个候选分组。
type topK struct {
	h resultHeap
	k int
}

func newTopK(k int) *topK {
	return &topK{h: resultHeap{items: make([]*GroupResult, 0, k)}, k: k}
}

func (t *topK) offer(g *GroupResult) {
	if t.h.Len() < t.k {
		heap.Push(&t.h, g)
		return
	}
	worst := t.h.items[0]
	if compareGroup(g.Key, g.Avg, worst.Key, worst.Avg) < 0 {
		heap.Pop(&t.h)
		heap.Push(&t.h, g)
	}
}

// sorted 返回按排行顺序排列的全部留存结果。
func (t *topK) sorted() []GroupResult {
	out := make([]GroupResult, 0, t.h.Len())
	for t.h.Len() > 0 {
		out = append(out, *heap.Pop(&t.h).(*GroupResult))
	}
	// heap.Pop 按“最差优先”弹出，反转即为排行顺序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
