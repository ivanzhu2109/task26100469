package kpirank

import (
	"bufio"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// 派生文件格式：
//
//	magic(8) = "KPIRANK1"
//	uint32 列数
//	每列: uint16 名称长度 + 名称字节
//	uint32 指标列下标
//	uint64 记录数（载入结束时回填）
//	每条记录: 1 字节指标空标志(0/1)，非空时 8 字节小端 int64，
//	          随后每个非指标列: uint16 长度 + 字节
var magic = []byte("KPIRANK1")

const maxCellBytes = 256

// Load 载入本地 CSV 文件，生成派生数据文件并返回数据集句柄。
// metric 为指标列名；其余列均为候选维度。派生文件写在源文件旁，
// 命名为 "<源文件>.kpirank"。
func Load(csvPath, metric string) (*Dataset, error) {
	src, err := os.Open(csvPath)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	r := csv.NewReader(bufio.NewReaderSize(src, 1<<20))
	r.ReuseRecord = true

	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("读取表头失败: %w", err)
	}
	// ReuseRecord 会复用底层数组，立即拷贝表头
	header = append([]string(nil), header...)
	seen := make(map[string]bool, len(header))
	metricCol := -1
	for i, name := range header {
		if seen[name] {
			return nil, fmt.Errorf("列名重复: %q", name)
		}
		seen[name] = true
		if name == metric {
			metricCol = i
		}
	}
	if metricCol < 0 {
		return nil, fmt.Errorf("指标列不存在: %q", metric)
	}

	dataFile := csvPath + ".kpirank"
	out, err := os.Create(dataFile)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriterSize(out, 1<<20)

	w.Write(magic)
	writeU32(w, uint32(len(header)))
	for _, name := range header {
		writeU16(w, uint16(len(name)))
		w.WriteString(name)
	}
	writeU32(w, uint32(metricCol))
	writeU64(w, 0) // 记录数占位，结束时回填

	var rows int64
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			out.Close()
			os.Remove(dataFile)
			return nil, fmt.Errorf("第 %d 条记录解析失败: %w", rows+1, err)
		}
		m := rec[metricCol]
		if m == "" {
			w.WriteByte(1)
		} else {
			v, perr := strconv.ParseInt(m, 10, 64)
			if perr != nil {
				out.Close()
				os.Remove(dataFile)
				return nil, fmt.Errorf("第 %d 条记录指标非法: %q", rows+1, m)
			}
			w.WriteByte(0)
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], uint64(v))
			w.Write(b[:])
		}
		for i, cell := range rec {
			if i == metricCol {
				continue
			}
			if len(cell) > maxCellBytes {
				out.Close()
				os.Remove(dataFile)
				return nil, fmt.Errorf("第 %d 条记录第 %d 列超过 %d 字节", rows+1, i+1, maxCellBytes)
			}
			writeU16(w, uint16(len(cell)))
			w.WriteString(cell)
		}
		rows++
	}

	if err := w.Flush(); err != nil {
		out.Close()
		os.Remove(dataFile)
		return nil, err
	}
	// 回填记录数
	if _, err := out.Seek(int64(len(magic))+4, io.SeekStart); err != nil {
		out.Close()
		os.Remove(dataFile)
		return nil, err
	}
	for _, name := range header {
		if _, err := out.Seek(int64(2+len(name)), io.SeekCurrent); err != nil {
			out.Close()
			os.Remove(dataFile)
			return nil, err
		}
	}
	if _, err := out.Seek(4, io.SeekCurrent); err != nil { // 跳过指标列下标
		out.Close()
		os.Remove(dataFile)
		return nil, err
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(rows))
	if _, err := out.Write(b[:]); err != nil {
		out.Close()
		os.Remove(dataFile)
		return nil, err
	}
	if err := out.Close(); err != nil {
		os.Remove(dataFile)
		return nil, err
	}

	cols := make([]string, len(header))
	copy(cols, header)
	return &Dataset{
		dataFile:   dataFile,
		columns:    cols,
		metricCol:  metricCol,
		metricName: metric,
		rows:       rows,
	}, nil
}

// Close 释放数据集占用的资源（不删除派生文件）。
func (d *Dataset) Close() error {
	return nil
}

// RemoveDerived 删除派生数据文件。
func (d *Dataset) RemoveDerived() error {
	if d.dataFile == "" {
		return nil
	}
	err := os.Remove(d.dataFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeU16(w *bufio.Writer, v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	w.Write(b[:])
}

func writeU32(w *bufio.Writer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.Write(b[:])
}

func writeU64(w *bufio.Writer, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.Write(b[:])
}

// derivedReader 流式读取派生数据文件。
type derivedReader struct {
	f         *os.File
	r         *bufio.Reader
	columns   []string
	metricCol int
	rows      int64
}

func openDerived(path string) (*derivedReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	dr := &derivedReader{f: f, r: r}
	m := make([]byte, len(magic))
	if _, err := io.ReadFull(r, m); err != nil || string(m) != string(magic) {
		f.Close()
		return nil, errors.New("派生文件格式非法")
	}
	ncol, err := readU32(r)
	if err != nil {
		f.Close()
		return nil, err
	}
	dr.columns = make([]string, ncol)
	for i := range dr.columns {
		n, err := readU16(r)
		if err != nil {
			f.Close()
			return nil, err
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			f.Close()
			return nil, err
		}
		dr.columns[i] = string(buf)
	}
	mc, err := readU32(r)
	if err != nil {
		f.Close()
		return nil, err
	}
	dr.metricCol = int(mc)
	rows, err := readU64(r)
	if err != nil {
		f.Close()
		return nil, err
	}
	dr.rows = int64(rows)
	return dr, nil
}

func (dr *derivedReader) close() {
	dr.f.Close()
}

// next 读取下一条记录。dims 按 want 指定的列下标顺序填充（复用内部缓冲，
// 仅在下次调用前有效）。返回 io.EOF 表示结束。
func (dr *derivedReader) next(want []int, dims [][]byte) (null bool, val int64, err error) {
	flag, err := dr.r.ReadByte()
	if err != nil {
		return false, 0, err
	}
	null = flag == 1
	if !null {
		var b [8]byte
		if _, err := io.ReadFull(dr.r, b[:]); err != nil {
			return false, 0, err
		}
		val = int64(binary.LittleEndian.Uint64(b[:]))
	}
	wi := 0
	for i := 0; i < len(dr.columns); i++ {
		if i == dr.metricCol {
			continue
		}
		n, err := readU16(dr.r)
		if err != nil {
			return false, 0, err
		}
		if wi < len(want) && want[wi] == i {
			if cap(dims[wi]) < int(n) {
				dims[wi] = make([]byte, n)
			}
			dims[wi] = dims[wi][:n]
			if _, err := io.ReadFull(dr.r, dims[wi]); err != nil {
				return false, 0, err
			}
			wi++
		} else {
			if _, err := dr.r.Discard(int(n)); err != nil {
				return false, 0, err
			}
		}
	}
	return null, val, nil
}

func readU16(r *bufio.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b[:]), nil
}

func readU32(r *bufio.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func readU64(r *bufio.Reader) (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}
