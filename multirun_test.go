package kpirank

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
)

// TestMultiRunMerge 调小顺串内存预算，强制外部排序走多顺串归并，
// 结果仍须与参考实现一致。
func TestMultiRunMerge(t *testing.T) {
	old := runMemBudget
	runMemBudget = 4096 // 极小预算，产生大量顺串
	defer func() { runMemBudget = old }()

	header := []string{"customer", "product", "metric"}
	rng := rand.New(rand.NewSource(7))
	var records [][]string
	for i := 0; i < 20000; i++ {
		m := ""
		if rng.Intn(5) != 0 {
			m = strconv.Itoa(rng.Intn(4000) - 2000)
		}
		records = append(records, []string{
			fmt.Sprintf("cust-%04d", rng.Intn(3000)),
			fmt.Sprintf("prod-%03d", rng.Intn(500)),
			m,
		})
	}
	csvPath := writeCSV(t, header, records)
	for _, dims := range [][]string{{"customer"}, {"customer", "product"}} {
		got := loadAndRank(t, csvPath, dims, 5000)
		want := refRank(t, csvPath, dims, 5000)
		assertSameRanking(t, got, want)
	}
}
