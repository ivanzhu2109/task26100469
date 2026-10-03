package kpirank

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// rowsFileName 是行存储文件名。
const rowsFileName = "rows.bin"

// rowStore 是记录的行式本地文件存储。
// 格式：每条记录为连续的单元格序列，每个单元格为
// uvarint 字节长度 + 原始字节。行之间无分隔，
// 由调用方保证每条记录的单元格数一致。
type rowStore struct {
	path   string
	file   *os.File
	writer *bufio.Writer
	nCols  int
	nRows  int64
	tmp    []byte // 编码缓冲
}

// openRowStore 在 dir 下创建新的行存储（已存在则截断）。
func openRowStore(dir string, nCols int) (*rowStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, rowsFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &rowStore{
		path:   path,
		file:   f,
		writer: bufio.NewWriterSize(f, 1<<16),
		nCols:  nCols,
		tmp:    make([]byte, 0, binary.MaxVarintLen64*(nCols+1)),
	}, nil
}

// appendRow 追加一条记录（已按列拆分）。
func (s *rowStore) appendRow(cells []string) error {
	if len(cells) != s.nCols {
		return errors.New("kpirank: row column count mismatch")
	}
	buf := s.tmp[:0]
	var ln [binary.MaxVarintLen64]byte
	for _, c := range cells {
		n := binary.PutUvarint(ln[:], uint64(len(c)))
		buf = append(buf, ln[:n]...)
		buf = append(buf, c...)
	}
	if _, err := s.writer.Write(buf); err != nil {
		return err
	}
	s.nRows++
	return nil
}

// close 冲刷并关闭存储。
func (s *rowStore) close() error {
	if s.writer != nil {
		if err := s.writer.Flush(); err != nil {
			s.file.Close()
			return err
		}
		s.writer = nil
	}
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

// remove 关闭并删除存储文件。
func (s *rowStore) remove() error {
	if err := s.close(); err != nil {
		return err
	}
	return os.Remove(s.path)
}

// rowReader 从行存储顺序读取记录。
type rowReader struct {
	file   *os.File
	reader *bufio.Reader
	row    []string // 复用的行缓冲
	data   []byte   // 复用的单元格字节缓冲
}

// openRowReader 打开存储文件的顺序读取器，nCols 为每行单元格数。
func (s *rowStore) openRowReader() (*rowReader, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	return &rowReader{
		file:   f,
		reader: bufio.NewReaderSize(f, 1<<16),
		row:    make([]string, s.nCols),
	}, nil
}

// next 读取下一条记录到内部缓冲；返回 io.EOF 表示结束。
// 返回的切片仅在下一次 next 调用前有效。
func (r *rowReader) next() ([]string, error) {
	for i := range r.row {
		ln, err := binary.ReadUvarint(r.reader)
		if err != nil {
			if err == io.EOF && i == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		}
		if cap(r.data) < int(ln) {
			r.data = make([]byte, ln)
		}
		data := r.data[:ln]
		if _, err := io.ReadFull(r.reader, data); err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		r.row[i] = string(data)
	}
	return r.row, nil
}

// close 关闭读取器。
func (r *rowReader) close() error {
	if r.file != nil {
		err := r.file.Close()
		r.file = nil
		return err
	}
	return nil
}
