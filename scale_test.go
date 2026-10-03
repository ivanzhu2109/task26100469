package kpirank

import (
	"bufio"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 规模参数（验收要求）
const (
	scaleRows   = 500000
	scaleGroups = 450000
	scaleLimit  = 100
)

// 特殊分组下标
const (
	idxEarlyLeader = 0
	idxLateBloomer = 200000
	idxEmptyStart  = scaleGroups - 10 // 最后 10 组全部指标为空
)

// dimValue 生成确定性的 256 字节维度值。
func dimValue(prefix byte, idx int) string {
	if idx < 0 {
		return strings.Repeat(string(prefix), 256)
	}
	return strings.Repeat(string(prefix), 250) + fmt.Sprintf("%06d", idx)
}

// groupKey 返回第 i 个分组的 (d1, d2, d3) 键；d3 的字节序与 i 一致。
func groupKey(i int) [3]string {
	return [3]string{dimValue('x', -1), dimValue('y', -1), dimValue('z', i)}
}

// TestScale 可复现的规模测试：
//   - 500000 行、450000 个分组、单组维度值合计 768 字节；
//   - late-bloomer 组前期指标极差，因文件末尾一条记录进入榜首；
//   - early-leader 组开头是榜首，因后续记录跌出榜单；
//   - 核对最终排名，并报告峰值内存与耗时（断言 <128MiB、<120s）。
func TestScale(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试在 -short 模式下跳过")
	}
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "scale.csv")

	// ---- 生成测试数据（不计时）----
	genStart := time.Now()
	func() {
		f, err := os.Create(csvPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		w := bufio.NewWriterSize(f, 4<<20)
		defer w.Flush()
		w.WriteString("d1,d2,d3,metric\n")
		writeRow := func(idx int, metric string) {
			k := groupKey(idx)
			w.WriteString(k[0])
			w.WriteByte(',')
			w.WriteString(k[1])
			w.WriteByte(',')
			w.WriteString(k[2])
			w.WriteByte(',')
			w.WriteString(metric)
			w.WriteByte('\n')
		}
		// 行 0：early-leader 首条，巨额正指标，开局即榜首
		writeRow(idxEarlyLeader, "1000000")
		// 行 1..449999：组 1..449999 的基线记录
		for i := 1; i < scaleGroups; i++ {
			switch {
			case i == idxLateBloomer:
				// late-bloomer 首条：极差指标，前期排名垫底
				writeRow(i, "-1000000000")
			case i >= idxEmptyStart:
				// 空指标组：全部记录指标为空
				writeRow(i, "")
			default:
				writeRow(i, "0")
			}
		}
		// 9 条 early-leader 后续极差记录，使其跌出榜单
		for i := 0; i < 9; i++ {
			writeRow(idxEarlyLeader, "-1000000")
		}
		// 填充至 scaleRows-1 行：普通组指标 0，不改变任何 avg
		filler := scaleRows - 1 - (1 + (scaleGroups - 1) + 9)
		for n := 0; n < filler; n++ {
			j := 1 + n%(idxEmptyStart-2)
			if j == idxLateBloomer {
				j = 1
			}
			writeRow(j, "0")
		}
		// 最后一行：late-bloomer 的翻盘记录
		writeRow(idxLateBloomer, "1000000000000000")
	}()
	t.Logf("数据生成耗时 %v（不计入考核）", time.Since(genStart))
	if fi, err := os.Stat(csvPath); err == nil {
		t.Logf("数据文件 %.1f MiB", float64(fi.Size())/(1<<20))
	}

	// ---- 载入 + 查询（计时与内存考核）----
	start := time.Now()
	ds, err := Load(csvPath, "metric")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.RemoveDerived()
	got, err := ds.Query(Query{Dimensions: []string{"d1", "d2", "d3"}, Limit: scaleLimit})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	peakMB := float64(ru.Maxrss) / (1 << 20)
	t.Logf("载入+查询耗时 %v，进程峰值驻留内存 %.1f MiB", elapsed, peakMB)
	if elapsed > 120*time.Second {
		t.Fatalf("耗时超限: %v > 120s", elapsed)
	}
	if peakMB > 128 {
		t.Fatalf("峰值内存超限: %.1f MiB > 128 MiB", peakMB)
	}

	// ---- 核对结果 ----
	if len(got) != scaleLimit {
		t.Fatalf("期望 %d 组，实际 %d", scaleLimit, len(got))
	}
	// 第 1 名：late-bloomer，avg = (10^15 - 10^9) / 2
	wantLate := groupKey(idxLateBloomer)
	if got[0].Key[2] != wantLate[2] {
		t.Fatalf("榜首应为 late-bloomer，实际键后缀 %q", got[0].Key[2][250:])
	}
	wantAvg := new(big.Rat).SetFrac(
		big.NewInt(1000000000000000-1000000000), big.NewInt(2))
	if got[0].Avg.Cmp(wantAvg) != 0 || got[0].Count != 2 {
		t.Fatalf("late-bloomer 统计错误: avg=%v count=%d", got[0].Avg, got[0].Count)
	}
	// 第 2..100 名：avg 0 的普通组，按键字节序（即下标）升序，
	// 跳过 early-leader(0) 与 late-bloomer(200000)
	next := 1
	for i := 1; i < scaleLimit; i++ {
		for next == idxEarlyLeader || next == idxLateBloomer {
			next++
		}
		want := groupKey(next)
		if got[i].Key[2] != want[2] {
			t.Fatalf("第 %d 名应为组 %d，实际键后缀 %q", i, next, got[i].Key[2][250:])
		}
		if got[i].Avg.Sign() != 0 {
			t.Fatalf("第 %d 名平均值应为 0，实际 %v", i, got[i].Avg)
		}
		next++
	}
	// early-leader 不在榜内
	earlyKey := groupKey(idxEarlyLeader)
	for _, g := range got {
		if g.Key[2] == earlyKey[2] {
			t.Fatal("early-leader 不应出现在榜单内")
		}
	}
	t.Logf("核对通过：late-bloomer 登顶，early-leader 跌出前 %d", scaleLimit)
}
