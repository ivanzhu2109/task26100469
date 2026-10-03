package kpirank

import (
	"bytes"
	"container/heap"
	"io"
	"math/big"
	"sort"
	"strconv"
)

// rowPairSource 从行存储生成 (分组键, 指标值) 对。
type rowPairSource struct {
	reader    *rowReader
	dimIdx    []int
	metricIdx int
	keyBuf    []byte
	items     []string
}

func (s *rowPairSource) next() (pair, error) {
	row, err := s.reader.next()
	if err != nil {
		return pair{}, err
	}
	for i, idx := range s.dimIdx {
		s.items[i] = row[idx]
	}
	s.keyBuf = encodeKey(s.items, s.keyBuf)
	p := pair{key: append([]byte(nil), s.keyBuf...)}
	cell := row[s.metricIdx]
	if cell != "" {
		v, perr := strconv.ParseInt(cell, 10, 64)
		if perr != nil {
			return pair{}, perr
		}
		p.valid = true
		p.val = v
	}
	return p, nil
}

func (s *rowPairSource) close() error { return s.reader.close() }

// decodeKey 将 encodeKey 的编码还原为维度元组（n 项）。
func decodeKey(enc []byte, n int) []string {
	items := make([]string, 0, n)
	var cur []byte
	for i := 0; i < len(enc); {
		if enc[i] == 0 {
			if i+1 < len(enc) && enc[i+1] == 1 {
				cur = append(cur, 0)
				i += 2
				continue
			}
			// 0x00 0x00：一项结束
			items = append(items, string(cur))
			cur = nil
			i += 2
			continue
		}
		cur = append(cur, enc[i])
		i++
	}
	return items
}

// rankEntry 是排名堆中的条目。
type rankEntry struct {
	keyEnc []byte   // 保序编码键（用于平局比较）
	key    []string // 解码后的维度元组
	count  int64
	sum    *big.Int
	avg    *big.Rat // 全部指标为空时为 nil
}

// better 报告 a 是否应排在 b 之前：
// 非空平均值优先；平均值高者优先；相等时键字节序小者优先。
func (e *rankEntry) better(o *rankEntry) bool {
	if e.avg == nil && o.avg == nil {
		return bytes.Compare(e.keyEnc, o.keyEnc) < 0
	}
	if e.avg == nil {
		return false
	}
	if o.avg == nil {
		return true
	}
	c := e.avg.Cmp(o.avg)
	if c != 0 {
		return c > 0
	}
	return bytes.Compare(e.keyEnc, o.keyEnc) < 0
}

// worstHeap 是小顶堆，堆顶为当前保留集合中“最差”的条目。
type worstHeap []*rankEntry

func (h worstHeap) Len() int { return len(h) }
func (h worstHeap) Less(i, j int) bool {
	// 堆顶应是最差者：b better a 时 a 更差，排在上面
	return h[j].better(h[i])
}
func (h worstHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *worstHeap) Push(x interface{}) { *h = append(*h, x.(*rankEntry)) }
func (h *worstHeap) Pop() interface{} {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return it
}

// groupRank 是 GroupRank 的内部实现：dimIdx 为维度列下标（1~3 个）。
func (d *Dataset) groupRank(dimIdx []int, topN int) ([]GroupResult, error) {
	reader, err := d.store.openRowReader()
	if err != nil {
		return nil, err
	}
	src := &rowPairSource{
		reader:    reader,
		dimIdx:    dimIdx,
		metricIdx: d.metricIdx,
		items:     make([]string, len(dimIdx)),
	}
	sorted, err := externalSort(d.dir, src)
	if err != nil {
		return nil, err
	}
	defer sorted.close()

	h := &worstHeap{}
	consider := func(e *rankEntry) {
		if h.Len() < topN {
			heap.Push(h, e)
			return
		}
		if e.better((*h)[0]) {
			heap.Pop(h)
			heap.Push(h, e)
		}
	}

	var curKey []byte
	var curCount int64
	curSum := new(big.Int)
	tmp := new(big.Int)
	flushGroup := func() {
		if curKey == nil {
			return
		}
		e := &rankEntry{
			keyEnc: append([]byte(nil), curKey...),
			count:  curCount,
			sum:    new(big.Int).Set(curSum),
		}
		if curCount > 0 {
			e.avg = new(big.Rat).SetFrac(e.sum, big.NewInt(curCount))
		}
		consider(e)
	}

	for {
		p, err := sorted.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if curKey == nil || !bytes.Equal(p.key, curKey) {
			flushGroup()
			curKey = append(curKey[:0], p.key...)
			curCount = 0
			curSum.SetInt64(0)
		}
		if p.valid {
			curCount++
			curSum.Add(curSum, tmp.SetInt64(p.val))
		}
	}
	flushGroup()

	// 堆中即前 topN 组，按名次顺序弹出（堆是“最差在顶”，需先收集再排序）
	entries := make([]*rankEntry, 0, h.Len())
	for h.Len() > 0 {
		entries = append(entries, heap.Pop(h).(*rankEntry))
	}
	// 按名次升序排序
	sort.Slice(entries, func(i, j int) bool { return entries[i].better(entries[j]) })
	results := make([]GroupResult, len(entries))
	for i, e := range entries {
		results[i] = GroupResult{
			Key:   decodeKey(e.keyEnc, len(dimIdx)),
			Count: e.count,
			Sum:   e.sum,
			Avg:   e.avg,
		}
	}
	return results, nil
}
