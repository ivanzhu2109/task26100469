package kpirank

import (
	"math/big"
	"testing"
)

const maxI64 = "9223372036854775807"

// TestSumOverflowInt64 总和超出 int64 仍须精确。
func TestSumOverflowInt64(t *testing.T) {
	header := []string{"g", "metric"}
	records := [][]string{
		{"big", maxI64},
		{"big", maxI64},
		{"big", maxI64},
	}
	csvPath := writeCSV(t, header, records)
	got := loadAndRank(t, csvPath, []string{"g"}, 10)
	if len(got) != 1 {
		t.Fatalf("want 1 group, got %d", len(got))
	}
	g := got[0]
	// 3 * (2^63 - 1) = 27670116110564327421
	wantSum, _ := new(big.Int).SetString("27670116110564327421", 10)
	if g.Sum.Cmp(wantSum) != 0 {
		t.Fatalf("sum: got %s, want %s", g.Sum, wantSum)
	}
	if g.Count != 3 {
		t.Fatalf("count: got %d, want 3", g.Count)
	}
	wantAvg, _ := new(big.Int).SetString(maxI64, 10)
	if g.Avg.Num().Cmp(wantAvg) != 0 || g.Avg.Denom().Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("avg: got %s, want %s", g.Avg.RatString(), maxI64)
	}
}

// TestCloseAverages 两个平均值极接近但不相等，名次不得因舍入改变。
// A: avg = 2^63-1（单行 MaxInt64）
// B: avg = 2^63-1.5（MaxInt64 与 MaxInt64-2 两行，和超 int64）
// 二者差 0.5，float64 无法区分（均舍入到 2^63）。
func TestCloseAverages(t *testing.T) {
	header := []string{"g", "metric"}
	records := [][]string{
		{"b", maxI64},
		{"a", maxI64},
		{"b", "9223372036854775805"}, // MaxInt64 - 2
	}
	csvPath := writeCSV(t, header, records)
	got := loadAndRank(t, csvPath, []string{"g"}, 10)
	if len(got) != 2 {
		t.Fatalf("want 2 groups, got %d", len(got))
	}
	if got[0].Key[0] != "a" || got[1].Key[0] != "b" {
		t.Fatalf("order: got %q,%q want a,b", got[0].Key[0], got[1].Key[0])
	}
	// B 的和 = 2*(2^63-1) - 2 = 18446744073709551612，超 int64
	wantSumB, _ := new(big.Int).SetString("18446744073709551612", 10)
	if got[1].Sum.Cmp(wantSumB) != 0 {
		t.Fatalf("B sum: got %s, want %s", got[1].Sum, wantSumB)
	}
	// B 的平均值 = 9223372036854775806，恰比 A 小 1
	if got[1].Avg.RatString() != "9223372036854775806" {
		t.Fatalf("B avg: got %s", got[1].Avg.RatString())
	}
}

// TestCloseAveragesFraction 分数层面极接近：1/3 与 (2^62)/(3*2^62-1) 之类。
// X: sum=1,count=3 → 1/3
// Y: sum=3,count=10 → 3/10 = 0.3 < 1/3
// Z: sum=10,count=3 → 10/3
// 再加 W: sum=2,count=6 → 1/3，与 X 精确相等，按键序并列。
func TestCloseAveragesFraction(t *testing.T) {
	header := []string{"g", "metric"}
	var records [][]string
	add := func(g string, vals ...string) {
		for _, v := range vals {
			records = append(records, []string{g, v})
		}
	}
	add("x", "0", "0", "1")                                     // 1/3
	add("y", "1", "1", "1", "1", "1", "1", "1", "1", "1", "-6") // 3/10
	add("z", "3", "3", "4")                                     // 10/3
	add("w", "0", "0", "0", "0", "0", "2")                      // 2/6 = 1/3
	csvPath := writeCSV(t, header, records)
	got := loadAndRank(t, csvPath, []string{"g"}, 10)
	if len(got) != 4 {
		t.Fatalf("want 4 groups, got %d", len(got))
	}
	wantKeys := []string{"z", "w", "x", "y"} // w<x 字节序并列
	wantAvgs := []string{"10/3", "1/3", "1/3", "3/10"}
	for i := range wantKeys {
		if got[i].Key[0] != wantKeys[i] {
			t.Fatalf("rank %d: got %q, want %q", i, got[i].Key[0], wantKeys[i])
		}
		if got[i].Avg.RatString() != wantAvgs[i] {
			t.Fatalf("rank %d avg: got %s, want %s", i, got[i].Avg.RatString(), wantAvgs[i])
		}
	}
}

// TestNegativeBigSum 负数大总和精确。
func TestNegativeBigSum(t *testing.T) {
	header := []string{"g", "metric"}
	records := [][]string{
		{"neg", "-9223372036854775808"}, // MinInt64
		{"neg", "-9223372036854775808"},
		{"pos", "1"},
	}
	csvPath := writeCSV(t, header, records)
	got := loadAndRank(t, csvPath, []string{"g"}, 10)
	if len(got) != 2 {
		t.Fatalf("want 2 groups, got %d", len(got))
	}
	if got[0].Key[0] != "pos" || got[1].Key[0] != "neg" {
		t.Fatalf("order: got %q,%q", got[0].Key[0], got[1].Key[0])
	}
	wantSum, _ := new(big.Int).SetString("-18446744073709551616", 10)
	if got[1].Sum.Cmp(wantSum) != 0 {
		t.Fatalf("neg sum: got %s, want %s", got[1].Sum, wantSum)
	}
	if got[1].Avg.RatString() != "-9223372036854775808" {
		t.Fatalf("neg avg: got %s", got[1].Avg.RatString())
	}
}
