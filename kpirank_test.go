package kpirank

import (
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------- 测试辅助 ----------

// writeCSV 将表头与记录写入临时 CSV 文件。
func writeCSV(t *testing.T, header []string, records [][]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.csv")
	var sb strings.Builder
	sb.WriteString(strings.Join(header, ","))
	sb.WriteString("\n")
	for _, rec := range records {
		sb.WriteString(strings.Join(rec, ","))
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// refGroup 是独立参照实现的聚合结果。
type refGroup struct {
	key   []string
	count int64
	rows  int64
	sum   *big.Int
}

// reference 独立的全量精确计算：不经过库的任何内部代码。
func reference(header []string, records [][]string, metric string, dims []string, limit int) []GroupResult {
	mi := 0
	for i, h := range header {
		if h == metric {
			mi = i
		}
	}
	di := make([]int, len(dims))
	for j, d := range dims {
		for i, h := range header {
			if h == d {
				di[j] = i
			}
		}
	}
	groups := map[string]*refGroup{}
	for _, rec := range records {
		key := make([]string, len(di))
		for j, i := range di {
			key[j] = rec[i]
		}
		enc := strings.Join(key, "\x00")
		g := groups[enc]
		if g == nil {
			g = &refGroup{key: key, sum: new(big.Int)}
			groups[enc] = g
		}
		g.rows++
		if rec[mi] != "" {
			v, _ := strconv.ParseInt(rec[mi], 10, 64)
			g.sum.Add(g.sum, big.NewInt(v))
			g.count++
		}
	}
	out := make([]GroupResult, 0, len(groups))
	for _, g := range groups {
		r := GroupResult{Key: g.key, Count: g.count, Rows: g.rows, Sum: g.sum}
		if g.count > 0 {
			r.Avg = new(big.Rat).SetFrac(g.sum, big.NewInt(g.count))
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Avg == nil && b.Avg == nil {
			return itemsLess(a.Key, b.Key)
		}
		if a.Avg == nil {
			return false
		}
		if b.Avg == nil {
			return true
		}
		if c := a.Avg.Cmp(b.Avg); c != 0 {
			return c > 0
		}
		return itemsLess(a.Key, b.Key)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func itemsLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// assertResults 断言库结果与参照结果完全一致。
func assertResults(t *testing.T, got, want []GroupResult) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("结果数量不一致: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		if strings.Join(g.Key, "|") != strings.Join(w.Key, "|") {
			t.Fatalf("第 %d 名键不一致: got %v, want %v", i, g.Key, w.Key)
		}
		if g.Count != w.Count || g.Rows != w.Rows {
			t.Fatalf("第 %d 名计数不一致: got (%d,%d), want (%d,%d)", i, g.Count, g.Rows, w.Count, w.Rows)
		}
		if g.Sum.Cmp(w.Sum) != 0 {
			t.Fatalf("第 %d 名总和不一致: got %v, want %v", i, g.Sum, w.Sum)
		}
		if (g.Avg == nil) != (w.Avg == nil) {
			t.Fatalf("第 %d 名平均值空性不一致", i)
		}
		if g.Avg != nil && g.Avg.Cmp(w.Avg) != 0 {
			t.Fatalf("第 %d 名平均值不一致: got %v, want %v", i, g.Avg, w.Avg)
		}
	}
}

// ---------- 用例 ----------

// TestSmallRank 小规模随机数据对照独立全量计算，覆盖重复分组、空指标、
// 空字符串维度、大小写敏感、空白保留。
func TestSmallRank(t *testing.T) {
	header := []string{"region", "product", "channel", "amount"}
	rng := rand.New(rand.NewSource(42))
	regions := []string{"cn", "CN", " cn", "cn ", "", "us"}
	products := []string{"apple", "Apple", "apple ", "", "banana"}
	channels := []string{"web", "app", "", " web"}
	var records [][]string
	for i := 0; i < 3000; i++ {
		m := ""
		if rng.Intn(4) != 0 {
			m = strconv.Itoa(rng.Intn(2001) - 1000)
		}
		records = append(records, []string{
			regions[rng.Intn(len(regions))],
			products[rng.Intn(len(products))],
			channels[rng.Intn(len(channels))],
			m,
		})
	}
	path := writeCSV(t, header, records)
	ds, err := Load(path, "amount")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.RemoveDerived()

	for _, dims := range [][]string{
		{"region"},
		{"product", "channel"},
		{"region", "product", "channel"},
	} {
		for _, limit := range []int{1, 7, 100, 10000} {
			got, err := ds.Query(Query{Dimensions: dims, Limit: limit})
			if err != nil {
				t.Fatal(err)
			}
			want := reference(header, records, "amount", dims, limit)
			assertResults(t, got, want)
		}
	}
}

// TestRowOrderInvariant 同一批记录改变行序，结果完全一致。
func TestRowOrderInvariant(t *testing.T) {
	header := []string{"cust", "sku", "qty"}
	rng := rand.New(rand.NewSource(7))
	var records [][]string
	for i := 0; i < 2000; i++ {
		m := ""
		if rng.Intn(5) != 0 {
			m = strconv.Itoa(rng.Intn(500))
		}
		records = append(records, []string{
			fmt.Sprintf("c%03d", rng.Intn(50)),
			fmt.Sprintf("s%02d", rng.Intn(10)),
			m,
		})
	}
	shuffled := make([][]string, len(records))
	perm := rng.Perm(len(records))
	for i, p := range perm {
		shuffled[i] = records[p]
	}
	q := Query{Dimensions: []string{"cust", "sku"}, Limit: 10000}

	ds1, err := Load(writeCSV(t, header, records), "qty")
	if err != nil {
		t.Fatal(err)
	}
	defer ds1.RemoveDerived()
	r1, err := ds1.Query(q)
	if err != nil {
		t.Fatal(err)
	}

	ds2, err := Load(writeCSV(t, header, shuffled), "qty")
	if err != nil {
		t.Fatal(err)
	}
	defer ds2.RemoveDerived()
	r2, err := ds2.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	assertResults(t, r1, r2)
}

// TestBigSumsAndNearAvg 总和超出 int64、平均值极接近但不相等、精确并列。
func TestBigSumsAndNearAvg(t *testing.T) {
	max := strconv.FormatInt(math.MaxInt64, 10)
	maxm1 := strconv.FormatInt(math.MaxInt64-1, 10)
	header := []string{"g", "v"}
	records := [][]string{
		// A: sum = 2*MaxInt64，avg = MaxInt64（总和超出 int64 正半域）
		{"a", max}, {"a", max},
		// B: sum = 2*MaxInt64-1，avg = MaxInt64 - 0.5，与 A 差 0.5，
		//    float64 无法区分，必须精确比较
		{"b", max}, {"b", maxm1},
		// C: avg 与 A 精确相等（sum = MaxInt64），按键字节序排在 A 后
		{"c", max},
		// D: sum = 3*MaxInt64 > 2^63，avg = MaxInt64，与 A/C 并列，键最大排最后
		{"d", max}, {"d", max}, {"d", max},
	}
	path := writeCSV(t, header, records)
	ds, err := Load(path, "v")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.RemoveDerived()
	got, err := ds.Query(Query{Dimensions: []string{"g"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("期望 4 组，实际 %d", len(got))
	}
	order := []string{"a", "c", "d", "b"}
	for i, g := range got {
		if g.Key[0] != order[i] {
			t.Fatalf("第 %d 名应为 %q，实际 %q", i, order[i], g.Key[0])
		}
	}
	// 精确校验总和与平均值
	m := big.NewInt(math.MaxInt64)
	wantSum := map[string]*big.Int{
		"a": new(big.Int).Mul(m, big.NewInt(2)),
		"b": new(big.Int).Sub(new(big.Int).Mul(m, big.NewInt(2)), big.NewInt(1)),
		"c": new(big.Int).Set(m),
		"d": new(big.Int).Mul(m, big.NewInt(3)),
	}
	for _, g := range got {
		if g.Sum.Cmp(wantSum[g.Key[0]]) != 0 {
			t.Fatalf("组 %q 总和错误: got %v, want %v", g.Key[0], g.Sum, wantSum[g.Key[0]])
		}
	}
	// b 的平均值 = MaxInt64 - 1/2
	wantAvgB := new(big.Rat).SetFrac(wantSum["b"], big.NewInt(2))
	if got[3].Avg.Cmp(wantAvgB) != 0 {
		t.Fatalf("组 b 平均值错误: got %v, want %v", got[3].Avg, wantAvgB)
	}
}

// TestEmptyMetricGroups 全部指标为空的组：计数与总和为零、平均值为空、
// 排在非空平均值之后，组间按维度字节序排列。
func TestEmptyMetricGroups(t *testing.T) {
	header := []string{"g", "v"}
	records := [][]string{
		{"z", ""}, {"z", ""},
		{"a", ""},
		{"m", "10"}, {"m", ""}, // m: 有效 1 条，avg 10
		{"n", "-5"},
	}
	path := writeCSV(t, header, records)
	ds, err := Load(path, "v")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.RemoveDerived()
	got, err := ds.Query(Query{Dimensions: []string{"g"}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("期望 4 组，实际 %d", len(got))
	}
	order := []string{"m", "n", "a", "z"}
	for i, g := range got {
		if g.Key[0] != order[i] {
			t.Fatalf("第 %d 名应为 %q，实际 %q", i, order[i], g.Key[0])
		}
	}
	// z: 全空
	if got[3].Count != 0 || got[3].Rows != 2 || got[3].Sum.Sign() != 0 || got[3].Avg != nil {
		t.Fatalf("组 z 统计错误: %+v", got[3])
	}
	// m: 空指标记录仍计入分组行数
	if got[0].Count != 1 || got[0].Rows != 2 {
		t.Fatalf("组 m 计数错误: %+v", got[0])
	}
}

// TestValidation 输入与参数校验。
func TestValidation(t *testing.T) {
	dir := t.TempDir()
	// 非法指标
	bad := filepath.Join(dir, "bad.csv")
	os.WriteFile(bad, []byte("g,v\na,xyz\n"), 0o644)
	if _, err := Load(bad, "v"); err == nil {
		t.Fatal("非法指标应报错")
	}
	// 重复列名
	dup := filepath.Join(dir, "dup.csv")
	os.WriteFile(dup, []byte("g,g,v\na,b,1\n"), 0o644)
	if _, err := Load(dup, "v"); err == nil {
		t.Fatal("重复列名应报错")
	}
	// 指标列不存在
	ok := filepath.Join(dir, "ok.csv")
	os.WriteFile(ok, []byte("g,v\na,1\n"), 0o644)
	if _, err := Load(ok, "nope"); err == nil {
		t.Fatal("指标列不存在应报错")
	}
	// 超长单元格
	long := filepath.Join(dir, "long.csv")
	os.WriteFile(long, []byte("g,v\n"+strings.Repeat("x", 257)+",1\n"), 0o644)
	if _, err := Load(long, "v"); err == nil {
		t.Fatal("超长单元格应报错")
	}
	// 查询参数
	ds, err := Load(ok, "v")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.RemoveDerived()
	if _, err := ds.Query(Query{Dimensions: nil, Limit: 1}); err == nil {
		t.Fatal("空维度应报错")
	}
	if _, err := ds.Query(Query{Dimensions: []string{"g", "g"}, Limit: 1}); err == nil {
		t.Fatal("重复维度应报错")
	}
	if _, err := ds.Query(Query{Dimensions: []string{"v"}, Limit: 1}); err == nil {
		t.Fatal("指标列作维度应报错")
	}
	if _, err := ds.Query(Query{Dimensions: []string{"g"}, Limit: 0}); err == nil {
		t.Fatal("Limit 0 应报错")
	}
	if _, err := ds.Query(Query{Dimensions: []string{"g"}, Limit: 10001}); err == nil {
		t.Fatal("Limit 10001 应报错")
	}
	if _, err := ds.Query(Query{Dimensions: []string{"ghost"}, Limit: 1}); err == nil {
		t.Fatal("不存在的维度应报错")
	}
}
