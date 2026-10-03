package kpirank

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"math/big"
	"os"
	"path/filepath"
)

// 桶文件中的条目格式：
//   uint16 键编码长度 + 键编码（每个维度: uint16 长度 + 字节）
//   1 字节指标空标志(0/1)，非空时 8 字节小端 int64

const numBuckets = 256

// groupAgg 是单个分组的聚合状态。
type groupAgg struct {
	items []string // 维度值元组（查询维度顺序），与编码键共享底层存储
	count int64    // 有效计数
	rows  int64    // 总记录数
	sum   *big.Int // 有效指标总和
}

// encodeKey 将维度值元组编码为长度前缀字节串，追加到 dst。
func encodeKey(dst []byte, dims [][]byte) []byte {
	var b [2]byte
	for _, d := range dims {
		binary.LittleEndian.PutUint16(b[:], uint16(len(d)))
		dst = append(dst, b[:]...)
		dst = append(dst, d...)
	}
	return dst
}

// decodeKey 解码长度前缀键，返回的字符串与 enc 共享底层存储。
func decodeKey(enc string) []string {
	var items []string
	for len(enc) > 0 {
		n := int(binary.LittleEndian.Uint16([]byte(enc[:2])))
		items = append(items, enc[2:2+n])
		enc = enc[2+n:]
	}
	return items
}

// partition 流式读取派生文件，将 (键, 指标) 条目按哈希分桶写入临时文件。
// wantCol 为选中的维度列下标（升序）。返回临时目录与桶文件路径。
func partition(dr *derivedReader, wantCol []int, tmpDir string) ([]string, error) {
	paths := make([]string, numBuckets)
	files := make([]*os.File, numBuckets)
	writers := make([]*bufio.Writer, numBuckets)
	for i := 0; i < numBuckets; i++ {
		p := filepath.Join(tmpDir, fmt.Sprintf("b%03d", i))
		f, err := os.Create(p)
		if err != nil {
			for j := 0; j < i; j++ {
				writers[j].Flush()
				files[j].Close()
			}
			return nil, err
		}
		paths[i] = p
		files[i] = f
		writers[i] = bufio.NewWriterSize(f, 32<<10)
	}

	dims := make([][]byte, len(wantCol))
	key := make([]byte, 0, 1024)
	h := fnv.New64a()
	var hb [8]byte
	for {
		null, val, err := dr.next(wantCol, dims)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		key = encodeKey(key[:0], dims)
		h.Reset()
		h.Write(key)
		bucket := h.Sum64() % numBuckets
		w := writers[bucket]
		binary.LittleEndian.PutUint16(hb[:2], uint16(len(key)))
		w.Write(hb[:2])
		w.Write(key)
		if null {
			w.WriteByte(1)
		} else {
			w.WriteByte(0)
			binary.LittleEndian.PutUint64(hb[:8], uint64(val))
			w.Write(hb[:8])
		}
	}
	for i := range writers {
		if err := writers[i].Flush(); err != nil {
			return nil, err
		}
		if err := files[i].Close(); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// aggregateBucket 在内存中聚合一个桶文件，对每个分组调用 emit。
func aggregateBucket(path string, emit func(enc string, g *groupAgg)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)

	groups := make(map[string]*groupAgg)
	var tmp big.Int
	var kbuf []byte
	for {
		var nb [2]byte
		if _, err := io.ReadFull(r, nb[:]); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		klen := int(binary.LittleEndian.Uint16(nb[:]))
		// 键编码最大为 3 个维度 * (2 + 256) 字节
		if cap(kbuf) < klen {
			kbuf = make([]byte, klen)
		}
		kbuf = kbuf[:klen]
		if _, err := io.ReadFull(r, kbuf); err != nil {
			return err
		}
		flag, err := r.ReadByte()
		if err != nil {
			return err
		}
		var val int64
		if flag == 0 {
			var vb [8]byte
			if _, err := io.ReadFull(r, vb[:]); err != nil {
				return err
			}
			val = int64(binary.LittleEndian.Uint64(vb[:]))
		}
		key := string(kbuf)
		g := groups[key]
		if g == nil {
			g = &groupAgg{items: decodeKey(key), sum: new(big.Int)}
			groups[key] = g
		}
		g.rows++
		if flag == 0 {
			g.count++
			g.sum.Add(g.sum, tmp.SetInt64(val))
		}
	}
	for enc, g := range groups {
		emit(enc, g)
	}
	return nil
}
