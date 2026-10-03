package kpirank

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// runMemBudget 是单个排序顺串的内存预算（字节）。
// 取 16 MiB：Go GC 默认在堆增长到约 2 倍存活对象时回收，
// 该预算配合查询期其它开销，使进程峰值相对 128 MiB 限制留有充足余量。
// 定义为变量以便测试调小强制多顺串归并。
var runMemBudget = 16 << 20

// pair 是待排序的 (分组键, 指标值) 记录。
type pair struct {
	key   []byte // 保序编码后的维度元组
	valid bool   // 指标是否非空
	val   int64  // 指标值（valid 时有效）
}

// encodeKey 将维度元组编码为保序字节串：
// 逐字节输出，0x00 转义为 0x00 0x01，每项以 0x00 0x00 结尾。
// 编码后的字节序与“逐项 UTF-8 字节序”的元组序一致。
func encodeKey(items []string, buf []byte) []byte {
	buf = buf[:0]
	for _, it := range items {
		for i := 0; i < len(it); i++ {
			b := it[i]
			if b == 0 {
				buf = append(buf, 0, 1)
			} else {
				buf = append(buf, b)
			}
		}
		buf = append(buf, 0, 0)
	}
	return buf
}

// pairSource 是 pair 的顺序来源。
type pairSource interface {
	next() (pair, error) // 返回 io.EOF 表示结束
	close() error
}

// pairWriter 将 pair 流式写入临时文件。
type pairWriter struct {
	file   *os.File
	writer *bufio.Writer
	tmp    []byte
}

func newPairWriter(path string) (*pairWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &pairWriter{
		file:   f,
		writer: bufio.NewWriterSize(f, 1<<16),
		tmp:    make([]byte, 0, 512),
	}, nil
}

// 编码：uvarint keyLen | key | 1 字节 valid | valid 时 varint(zigzag) val
func (w *pairWriter) write(p pair) error {
	buf := w.tmp[:0]
	var ln [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(ln[:], uint64(len(p.key)))
	buf = append(buf, ln[:n]...)
	buf = append(buf, p.key...)
	if p.valid {
		buf = append(buf, 1)
		n = binary.PutVarint(ln[:], p.val)
		buf = append(buf, ln[:n]...)
	} else {
		buf = append(buf, 0)
	}
	_, err := w.writer.Write(buf)
	return err
}

func (w *pairWriter) close() error {
	if w.writer != nil {
		if err := w.writer.Flush(); err != nil {
			w.file.Close()
			return err
		}
		w.writer = nil
	}
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// pairReader 从临时文件流式读取 pair。
type pairReader struct {
	file   *os.File
	reader *bufio.Reader
	key    []byte // 复用的键缓冲
}

func newPairReader(path string) (*pairReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &pairReader{
		file:   f,
		reader: bufio.NewReaderSize(f, 1<<16),
	}, nil
}

func (r *pairReader) next() (pair, error) {
	var p pair
	kl, err := binary.ReadUvarint(r.reader)
	if err != nil {
		if err == io.EOF {
			return p, io.EOF
		}
		return p, err
	}
	if cap(r.key) < int(kl) {
		r.key = make([]byte, kl)
	}
	r.key = r.key[:kl]
	if _, err := io.ReadFull(r.reader, r.key); err != nil {
		return p, err
	}
	vb, err := r.reader.ReadByte()
	if err != nil {
		return p, err
	}
	p.key = r.key
	if vb == 1 {
		p.valid = true
		v, err := binary.ReadVarint(r.reader)
		if err != nil {
			return p, err
		}
		p.val = v
	}
	return p, nil
}

func (r *pairReader) close() error {
	if r.file != nil {
		err := r.file.Close()
		r.file = nil
		return err
	}
	return nil
}

// run 是内存中的一个顺串。
type run struct {
	pairs []pair
	arena []byte
	mem   int
}

// mergeReader 对多个已排序来源做 k 路归并。
type mergeReader struct {
	h     pairHeap
	items []mergeItem
}

type mergeItem struct {
	p   pair
	src int
}

// pairHeap 是按 key 升序的最小堆。
type pairHeap []mergeItem

func (h pairHeap) Len() int { return len(h) }
func (h pairHeap) Less(i, j int) bool {
	return bytes.Compare(h[i].p.key, h[j].p.key) < 0
}
func (h pairHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *pairHeap) Push(x interface{}) { *h = append(*h, x.(mergeItem)) }
func (h *pairHeap) Pop() interface{} {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// externalSort 从 src 读入全部 pair，外部排序后返回按 key 升序的来源。
// 临时文件写入 dir，调用方负责在消费完毕后 close（并删除临时文件）。
func externalSort(dir string, src pairSource) (pairSource, error) {
	defer src.close()
	var runs []string
	cur := run{arena: make([]byte, 0, 1<<20)}
	flush := func() error {
		if len(cur.pairs) == 0 {
			return nil
		}
		sort.Slice(cur.pairs, func(i, j int) bool {
			return bytes.Compare(cur.pairs[i].key, cur.pairs[j].key) < 0
		})
		path := filepath.Join(dir, "run-"+strconv.Itoa(len(runs))+".tmp")
		w, err := newPairWriter(path)
		if err != nil {
			return err
		}
		for _, p := range cur.pairs {
			if err := w.write(p); err != nil {
				w.close()
				return err
			}
		}
		if err := w.close(); err != nil {
			return err
		}
		runs = append(runs, path)
		cur = run{arena: make([]byte, 0, 1<<20)}
		return nil
	}
	for {
		p, err := src.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			removeRuns(runs)
			return nil, err
		}
		off := len(cur.arena)
		cur.arena = append(cur.arena, p.key...)
		p.key = cur.arena[off:]
		cur.pairs = append(cur.pairs, p)
		cur.mem += len(p.key) + 64 // 键字节 + pair 结构开销估计
		if cur.mem >= runMemBudget {
			if err := flush(); err != nil {
				removeRuns(runs)
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		removeRuns(runs)
		return nil, err
	}
	switch len(runs) {
	case 0:
		return &emptySource{}, nil
	case 1:
		return &fileSource{path: runs[0]}, nil
	}
	// k 路归并
	mr := &mergeReader{}
	srcs := make([]pairSource, len(runs))
	for i, path := range runs {
		r, err := newPairReader(path)
		if err != nil {
			removeRuns(runs)
			return nil, err
		}
		srcs[i] = r
		p, err := r.next()
		if err == nil {
			mr.h = append(mr.h, mergeItem{p: p, src: i})
		} else if err != io.EOF {
			removeRuns(runs)
			return nil, err
		}
	}
	heap.Init(&mr.h)
	return &mergeSource{mr: mr, srcs: srcs, paths: runs}, nil
}

// emptySource 是空来源。
type emptySource struct{}

func (e *emptySource) next() (pair, error) { return pair{}, io.EOF }
func (e *emptySource) close() error        { return nil }

// fileSource 包装单个顺串文件，close 时删除文件。
type fileSource struct {
	path   string
	reader *pairReader
}

func (f *fileSource) next() (pair, error) {
	if f.reader == nil {
		r, err := newPairReader(f.path)
		if err != nil {
			return pair{}, err
		}
		f.reader = r
	}
	return f.reader.next()
}

func (f *fileSource) close() error {
	if f.reader != nil {
		f.reader.close()
		f.reader = nil
	}
	return os.Remove(f.path)
}

// mergeSource 是归并来源，close 时删除全部顺串文件。
type mergeSource struct {
	mr    *mergeReader
	srcs  []pairSource
	paths []string
}

func (m *mergeSource) next() (pair, error) {
	if m.mr.h.Len() == 0 {
		return pair{}, io.EOF
	}
	top := heap.Pop(&m.mr.h).(mergeItem)
	// top.p.key 与来源读取器共享底层缓冲，读取下一条前必须拷贝
	top.p.key = append([]byte(nil), top.p.key...)
	if p, err := m.srcs[top.src].next(); err == nil {
		heap.Push(&m.mr.h, mergeItem{p: p, src: top.src})
	}
	return top.p, nil
}

func (m *mergeSource) close() error {
	for _, s := range m.srcs {
		s.close()
	}
	removeRuns(m.paths)
	return nil
}

func removeRuns(paths []string) {
	for _, p := range paths {
		os.Remove(p)
	}
}
