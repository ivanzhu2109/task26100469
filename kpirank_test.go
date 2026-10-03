package kpirank

import (
	"encoding/csv"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// refGroup 是独立参考实现的分组聚合。
type refGroup struct {
	key   []string
	count int64
	sum   *big.Int
}

// refRank 用最直接的全量精确计算得到完整排名（参考实现）。
func refRank(t *testing.T, csvPath string, dims []string, topN int) []GroupResult {
	t.Helper()
	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	colIdx := map[string]int{}
	for i, c := range header {
		colIdx[c] = i
	}
	metricIdx := colIdx["metric"]
	dimIdx := make([]int, len(dims))
	for i, d := range dims {
		dimIdx[i] = colIdx[d]
	}
	groups := map[string]*refGroup{}
	keys := map[string][]string{}
	for {
		rec, err := r.Read()
		if err != nil {
			break
		}
		tuple := make([]string, len(dims))
		for i, idx := range dimIdx {
			tuple[i] = rec[idx]
		}
		gk := strings.Join(tuple, "\x1f")
		g, ok := groups[gk]
		if !ok {
			g = &refGroup{key: append([]string(nil), tuple...), sum: new(big.Int)}
			groups[gk] = g
			keys[gk] = tuple
		}
		if rec[metricIdx] != "" {
			v, err := strconv.ParseInt(rec[metricIdx], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			g.count++
			g.sum.Add(g.sum, big.NewInt(v))
		}
	}
	list := make([]*refGroup, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	avgOf := func(g *refGroup) *big.Rat {
		if g.count == 0 {
			return nil
		}
		return new(big.Rat).SetFrac(g.sum, big.NewInt(g.count))
	}
	sort.Slice(list, func(i, j int) bool {
		ai, aj := avgOf(list[i]), avgOf(list[j])
		if ai == nil && aj == nil {
			return tupleLess(list[i].key, list[j].key)
		}
		if ai == nil {
			return false
		}
		if aj == nil {
			return true
		}
		c := ai.Cmp(aj)
		if c != 0 {
			return c > 0
		}
		return tupleLess(list[i].key, list[j].key)
	})
	if len(list) > topN {
		list = list[:topN]
	}
	out := make([]GroupResult, len(list))
	for i, g := range list {
		out[i] = GroupResult{Key: g.key, Count: g.count, Sum: g.sum, Avg: avgOf(g)}
	}
	return out
}

// tupleLess 逐项 UTF-8 字节序比较元组。
func tupleLess(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// writeCSV 将表头与记录写入临时 CSV 文件。
func writeCSV(t *testing.T, header []string, records [][]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.csv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadAndRank 载入并排行。
func loadAndRank(t *testing.T, csvPath string, dims []string, topN int) []GroupResult {
	t.Helper()
	d, err := LoadCSV(t.TempDir(), csvPath, "metric")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Cleanup()
	res, err := d.GroupRank(dims, topN)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// assertSameRanking 比较两组排名结果完全一致。
func assertSameRanking(t *testing.T, got, want []GroupResult) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("result count: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		if strings.Join(g.Key, "\x1f") != strings.Join(w.Key, "\x1f") {
			t.Fatalf("rank %d key: got %q, want %q", i, g.Key, w.Key)
		}
		if g.Count != w.Count {
			t.Fatalf("rank %d count: got %d, want %d", i, g.Count, w.Count)
		}
		if g.Sum.Cmp(w.Sum) != 0 {
			t.Fatalf("rank %d sum: got %s, want %s", i, g.Sum, w.Sum)
		}
		if (g.Avg == nil) != (w.Avg == nil) {
			t.Fatalf("rank %d avg nil mismatch: got %v, want %v", i, g.Avg, w.Avg)
		}
		if g.Avg != nil && g.Avg.Cmp(w.Avg) != 0 {
			t.Fatalf("rank %d avg: got %s, want %s", i, g.Avg.RatString(), w.Avg.RatString())
		}
	}
}

// TestSmallRanking 小规模：重复分组、空指标、1~3 个维度、topN 超过分组数。
func TestSmallRanking(t *testing.T) {
	header := []string{"customer", "product", "region", "metric"}
	records := [][]string{
		{"alice", "apple", "east", "10"},
		{"bob", "apple", "east", "20"},
		{"alice", "apple", "west", "30"},
		{"alice", "banana", "east", ""},
		{"bob", "apple", "east", ""},
		{"carol", "banana", "west", "-5"},
		{"carol", "banana", "west", "5"},
		{"dave", "fig", "east", ""},
		{"dave", "fig", "east", ""},
		{"", "apple", "east", "7"},
		{"", "", "", ""},
		{"alice", "apple", "east", "1"},
	}
	csvPath := writeCSV(t, header, records)
	for _, dims := range [][]string{
		{"customer"},
		{"customer", "product"},
		{"customer", "product", "region"},
		{"region"},
	} {
		for _, topN := range []int{1, 3, 100} {
			got := loadAndRank(t, csvPath, dims, topN)
			want := refRank(t, csvPath, dims, topN)
			assertSameRanking(t, got, want)
		}
	}
}

// TestRowOrderInvariance 同一批记录任意打乱行序，结果完全一致。
func TestRowOrderInvariance(t *testing.T) {
	header := []string{"customer", "product", "metric"}
	rng := rand.New(rand.NewSource(42))
	var records [][]string
	customers := []string{"a", "b", "c", "d", "e", ""}
	for i := 0; i < 500; i++ {
		m := ""
		if rng.Intn(4) != 0 {
			m = strconv.Itoa(rng.Intn(2000) - 1000)
		}
		records = append(records, []string{
			customers[rng.Intn(len(customers))],
			"p" + strconv.Itoa(rng.Intn(7)),
			m,
		})
	}
	base := writeCSV(t, header, records)
	want := loadAndRank(t, base, []string{"customer", "product"}, 50)
	for trial := 0; trial < 5; trial++ {
		shuffled := append([][]string(nil), records...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		path := writeCSV(t, header, shuffled)
		got := loadAndRank(t, path, []string{"customer", "product"}, 50)
		assertSameRanking(t, got, want)
	}
}

// TestEmptyMetricGroups 全部指标为空的组：计数与总和为零、平均值为空、排在非空之后。
func TestEmptyMetricGroups(t *testing.T) {
	header := []string{"customer", "metric"}
	records := [][]string{
		{"empty1", ""},
		{"empty1", ""},
		{"full", "2"},
		{"empty2", ""},
		{"full", "4"},
	}
	csvPath := writeCSV(t, header, records)
	got := loadAndRank(t, csvPath, []string{"customer"}, 10)
	if len(got) != 3 {
		t.Fatalf("want 3 groups, got %d", len(got))
	}
	if got[0].Key[0] != "full" || got[0].Count != 2 || got[0].Sum.Int64() != 6 || got[0].Avg.RatString() != "3" {
		t.Fatalf("rank0 wrong: %+v", got[0])
	}
	// 空平均值组按维度字节序：empty1 < empty2
	if got[1].Key[0] != "empty1" || got[2].Key[0] != "empty2" {
		t.Fatalf("empty-avg order wrong: %q, %q", got[1].Key, got[2].Key)
	}
	for _, g := range got[1:] {
		if g.Count != 0 || g.Sum.Sign() != 0 || g.Avg != nil {
			t.Fatalf("empty group wrong: %+v", g)
		}
	}
}

// TestTieBreakByTuple 平均值精确相等时按维度元组字节序升序。
func TestTieBreakByTuple(t *testing.T) {
	header := []string{"customer", "metric"}
	records := [][]string{
		{"b", "10"},
		{"a", "10"},
		{"A", "10"}, // 大写按字节序排在小写前
		{"aa", "10"},
	}
	csvPath := writeCSV(t, header, records)
	got := loadAndRank(t, csvPath, []string{"customer"}, 10)
	wantOrder := []string{"A", "a", "aa", "b"}
	if len(got) != len(wantOrder) {
		t.Fatalf("want %d groups, got %d", len(wantOrder), len(got))
	}
	for i, w := range wantOrder {
		if got[i].Key[0] != w {
			t.Fatalf("rank %d: got %q, want %q", i, got[i].Key[0], w)
		}
	}
}

// TestValidation 参数校验。
func TestValidation(t *testing.T) {
	header := []string{"customer", "metric"}
	csvPath := writeCSV(t, header, [][]string{{"a", "1"}})
	d, err := LoadCSV(t.TempDir(), csvPath, "metric")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Cleanup()
	if _, err := d.GroupRank(nil, 1); err != ErrDimensionCount {
		t.Fatalf("want ErrDimensionCount, got %v", err)
	}
	if _, err := d.GroupRank([]string{"a", "b", "c", "d"}, 1); err != ErrDimensionCount {
		t.Fatalf("want ErrDimensionCount, got %v", err)
	}
	if _, err := d.GroupRank([]string{"customer"}, 0); err != ErrTopNRange {
		t.Fatalf("want ErrTopNRange, got %v", err)
	}
	if _, err := d.GroupRank([]string{"customer"}, 10001); err != ErrTopNRange {
		t.Fatalf("want ErrTopNRange, got %v", err)
	}
	if _, err := d.GroupRank([]string{"nosuch"}, 1); err == nil {
		t.Fatal("want error for unknown dimension")
	}
	if _, err := d.GroupRank([]string{"metric"}, 1); err == nil {
		t.Fatal("want error for metric as dimension")
	}
	if _, err := LoadCSV(t.TempDir(), csvPath, "nosuch"); err == nil {
		t.Fatal("want error for unknown metric column")
	}
}
