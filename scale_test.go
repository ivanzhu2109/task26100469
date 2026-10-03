package kpirank

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 规模测试常量：与需求上限一致。
const (
	scaleRows    = 500000
	scaleCols    = 100
	scaleGroups  = 450000
	scaleTopN    = 10000
	maxRSSBytes  = 128 << 20 // 128 MiB
	maxLoadQuery = 120 * time.Second
)

// groupName 生成第 i 个分组的维度值。
func groupName(i int) string {
	return fmt.Sprintf("g%06d", i)
}

// metricOf 返回第 i 行的指标值（空串表示空指标）。
// 数据场景：
//   - 阶段A：每组一行，指标 1；前 100 组指标 5000000（早期榜首）。
//   - 阶段B：前 100 组各追加一行 -10000000，平均值跌至 -2500000（跌出榜单）。
//   - 阶段C：末尾 100 组各追加一行 20000000，平均值升至 10000000.5（进入榜首）。
//   - 阶段D：其余行轮转分配到中间分组，偶数行指标 2、奇数行空指标。
func metricOf(i int) string {
	switch {
	case i < 100:
		return "5000000"
	case i < scaleGroups:
		return "1"
	case i < 450100:
		return "-10000000"
	case i < 450200:
		return "20000000"
	default:
		if (i-450200)%2 == 0 {
			return "2"
		}
		return ""
	}
}

// groupOf 返回第 i 行所属分组下标。
func groupOf(i int) int {
	switch {
	case i < scaleGroups:
		return i
	case i < 450100:
		return i - 450000 // g000000..g000099
	case i < 450200:
		return i - 450100 + 449900 // g449900..g449999
	default:
		return 200 + (i-450200)%(scaleGroups-300)
	}
}

// filler 是 20 字节的填充列值。
const filler = "abcdefghijklmnopqrst"

// generateScaleCSV 生成确定性的大规模 CSV（约 1 GiB）。
func generateScaleCSV(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "scale.csv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	var sb strings.Builder
	sb.WriteString("id,grp,metric")
	for c := 3; c < scaleCols; c++ {
		fmt.Fprintf(&sb, ",f%03d", c)
	}
	sb.WriteByte('\n')
	for i := 0; i < scaleRows; i++ {
		fmt.Fprintf(&sb, "%06d,%s,%s", i, groupName(groupOf(i)), metricOf(i))
		for c := 3; c < scaleCols; c++ {
			sb.WriteByte(',')
			sb.WriteString(filler)
		}
		sb.WriteByte('\n')
		if sb.Len() >= 1<<20 {
			if _, err := w.WriteString(sb.String()); err != nil {
				t.Fatal(err)
			}
			sb.Reset()
		}
	}
	if _, err := w.WriteString(sb.String()); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestScaleWorker 是子进程入口：执行载入+查询并输出结果与耗时。
// 通过环境变量启用，父进程测量其峰值内存。
func TestScaleWorker(t *testing.T) {
	if os.Getenv("KPIRANK_SCALE_WORKER") != "1" {
		t.Skip("worker mode only")
	}
	csvPath := os.Getenv("KPIRANK_SCALE_CSV")
	outPath := os.Getenv("KPIRANK_SCALE_OUT")
	dataDir := os.Getenv("KPIRANK_SCALE_DIR")

	t0 := time.Now()
	d, err := LoadCSV(dataDir, csvPath, "metric")
	if err != nil {
		t.Fatal(err)
	}
	loadDur := time.Since(t0)

	t1 := time.Now()
	res, err := d.GroupRank([]string{"grp"}, scaleTopN)
	if err != nil {
		t.Fatal(err)
	}
	queryDur := time.Since(t1)
	if err := d.Cleanup(); err != nil {
		t.Fatal(err)
	}

	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(out)
	for _, g := range res {
		avg := "-"
		if g.Avg != nil {
			avg = g.Avg.RatString()
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", strings.Join(g.Key, "\x1f"), g.Count, g.Sum.String(), avg)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("load_ms=%d query_ms=%d\n", loadDur.Milliseconds(), queryDur.Milliseconds())
}

// refScaleRank 父进程中的独立全量精确参考计算（仅流式聚合 grp 与 metric 两列）。
func refScaleRank(t *testing.T, csvPath string) []GroupResult {
	t.Helper()
	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.ReuseRecord = true
	header, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	grpIdx, metIdx := -1, -1
	for i, c := range header {
		switch c {
		case "grp":
			grpIdx = i
		case "metric":
			metIdx = i
		}
	}
	type agg struct {
		count int64
		sum   int64 // 本数据集设计为不溢出 int64
	}
	groups := make(map[string]*agg, scaleGroups)
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		g := groups[rec[grpIdx]]
		if g == nil {
			g = &agg{}
			groups[rec[grpIdx]] = g
		}
		if rec[metIdx] != "" {
			v, err := strconv.ParseInt(rec[metIdx], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			g.count++
			g.sum += v
		}
	}
	if len(groups) != scaleGroups {
		t.Fatalf("reference: want %d groups, got %d", scaleGroups, len(groups))
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		gi, gj := groups[keys[i]], groups[keys[j]]
		ai, aj := big.NewRat(gi.sum, max(gi.count, 1)), big.NewRat(gj.sum, max(gj.count, 1))
		ni, nj := gi.count == 0, gj.count == 0
		if ni && nj {
			return keys[i] < keys[j]
		}
		if ni {
			return false
		}
		if nj {
			return true
		}
		c := ai.Cmp(aj)
		if c != 0 {
			return c > 0
		}
		return keys[i] < keys[j]
	})
	if len(keys) > scaleTopN {
		keys = keys[:scaleTopN]
	}
	out := make([]GroupResult, len(keys))
	for i, k := range keys {
		g := groups[k]
		r := GroupResult{Key: []string{k}, Count: g.count, Sum: big.NewInt(g.sum)}
		if g.count > 0 {
			r.Avg = big.NewRat(g.sum, g.count)
		}
		out[i] = r
	}
	return out
}

// readWorkerResults 读取子进程输出的排名结果。
func readWorkerResults(t *testing.T, path string) []GroupResult {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []GroupResult
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			t.Fatalf("bad result line: %q", line)
		}
		count, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		sum, ok := new(big.Int).SetString(parts[2], 10)
		if !ok {
			t.Fatalf("bad sum: %q", parts[2])
		}
		g := GroupResult{Key: strings.Split(parts[0], "\x1f"), Count: count, Sum: sum}
		if parts[3] != "-" {
			r, ok := new(big.Rat).SetString(parts[3])
			if !ok {
				t.Fatalf("bad avg: %q", parts[3])
			}
			g.Avg = r
		}
		out = append(out, g)
	}
	return out
}

// TestScale 端到端规模测试：500000 行 × 100 列 ≈ 1 GiB，450000 个分组。
// 验证：末尾记录让前期靠后的组进入榜首、早期榜首跌出榜单；
// 子进程峰值内存 <= 128 MiB，载入+查询 <= 120 秒。
func TestScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short mode")
	}
	workDir, err := os.MkdirTemp("", "kpirank-scale")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)

	t0 := time.Now()
	csvPath := generateScaleCSV(t, workDir)
	genDur := time.Since(t0)
	fi, err := os.Stat(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("generated %d bytes (%.2f GiB) in %s", fi.Size(), float64(fi.Size())/(1<<30), genDur)

	outPath := filepath.Join(workDir, "result.txt")
	dataDir := filepath.Join(workDir, "data")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestScaleWorker$", "-test.v")
	cmd.Env = append(os.Environ(),
		"KPIRANK_SCALE_WORKER=1",
		"KPIRANK_SCALE_CSV="+csvPath,
		"KPIRANK_SCALE_OUT="+outPath,
		"KPIRANK_SCALE_DIR="+dataDir,
	)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	t1 := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("worker failed: %v", err)
	}
	wallDur := time.Since(t1)

	// 子进程峰值驻留内存
	ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok {
		t.Fatal("cannot get child rusage")
	}
	maxRSS := ru.Maxrss
	if runtime.GOOS == "linux" {
		maxRSS *= 1024 // Linux 以 KB 报告
	}
	t.Logf("child peak RSS: %d bytes (%.1f MiB)", maxRSS, float64(maxRSS)/(1<<20))
	t.Logf("worker wall time: %s; %s", wallDur, strings.TrimSpace(stdout.String()))
	if maxRSS > maxRSSBytes {
		t.Fatalf("peak RSS %d exceeds 128 MiB (%d)", maxRSS, maxRSSBytes)
	}

	// 解析子进程自报耗时
	var loadMs, queryMs int64
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "load_ms=") {
			if _, err := fmt.Sscanf(line, "load_ms=%d query_ms=%d", &loadMs, &queryMs); err != nil {
				t.Fatalf("cannot parse timing line %q: %v", line, err)
			}
		}
	}
	if loadMs == 0 && queryMs == 0 {
		t.Fatal("worker timing line not found in output")
	}
	total := time.Duration(loadMs+queryMs) * time.Millisecond
	t.Logf("load+query: %s (load %d ms, query %d ms)", total, loadMs, queryMs)
	if total > maxLoadQuery {
		t.Fatalf("load+query %s exceeds 120s", total)
	}

	// 与独立全量精确计算核对
	got := readWorkerResults(t, outPath)
	want := refScaleRank(t, csvPath)
	assertSameRanking(t, got, want)

	// 场景断言 1：文件末尾的记录让末尾 100 组占据榜首
	for i := 0; i < 100; i++ {
		wantKey := groupName(449900 + i)
		if got[i].Key[0] != wantKey {
			t.Fatalf("rank %d: got %q, want late riser %q", i, got[i].Key[0], wantKey)
		}
		if got[i].Count != 2 || got[i].Sum.String() != "20000001" {
			t.Fatalf("rank %d: got count=%d sum=%s", i, got[i].Count, got[i].Sum)
		}
	}
	// 场景断言 2：早期榜首（g000000..g000099）全部跌出榜单
	for _, g := range got {
		idx, err := strconv.Atoi(strings.TrimPrefix(g.Key[0], "g"))
		if err != nil {
			t.Fatal(err)
		}
		if idx < 100 {
			t.Fatalf("early leader %q should have dropped out of top %d", g.Key[0], scaleTopN)
		}
	}
	t.Logf("scenario verified: late records lifted g449900..g449999 to top 100; early leaders dropped out")
}
