package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-github/v57/github"
)

func newIssue(number int, state, created, closed string, pr bool, labels ...string) *github.Issue {
	i := &github.Issue{
		Number:    github.Int(number),
		State:     github.String(state),
		Title:     github.String("issue title"),
		Body:      github.String("body"),
		CreatedAt: &github.Timestamp{Time: mustParse(created)},
		User:      &github.User{Login: github.String("alice")},
		HTMLURL:   github.String("https://github.com/o/r/issues/1"),
		Comments:  github.Int(0),
	}
	if closed != "" {
		i.ClosedAt = &github.Timestamp{Time: mustParse(closed)}
	}
	if pr {
		i.PullRequestLinks = &github.PullRequestLinks{URL: github.String("https://api.github.com/o/r/pulls/1")}
	}
	for _, l := range labels {
		i.Labels = append(i.Labels, &github.Label{Name: github.String(l)})
	}
	return i
}

func mustParse(s string) time.Time {
	v, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return v
}

func TestBuildDashboardData_FiltersPullRequests(t *testing.T) {
	issues := []*github.Issue{
		newIssue(1, "open", "2024-01-01", "", false, "bug"),
		newIssue(2, "closed", "2024-01-02", "2024-01-05", true), // PR，应被过滤
		newIssue(3, "closed", "2024-02-01", "2024-02-10", false, "bug", "enhancement"),
	}

	d := BuildDashboardData("o", "r", issues)

	if d.Summary.Total != 2 {
		t.Fatalf("期望过滤掉 PR 后剩 2 个 issue，实际 %d", d.Summary.Total)
	}
	if len(d.Issues) != 2 {
		t.Fatalf("期望 issues 长度为 2，实际 %d", len(d.Issues))
	}
	if d.Meta.FullName != "o/r" {
		t.Fatalf("期望 FullName=o/r，实际 %s", d.Meta.FullName)
	}
}

func TestBuildDashboardData_Summary(t *testing.T) {
	issues := []*github.Issue{
		newIssue(1, "open", "2024-01-01", "", false),
		newIssue(2, "closed", "2024-01-01", "2024-01-11", false), // 10 天
		newIssue(3, "closed", "2024-01-01", "2024-01-21", false), // 20 天
	}

	d := BuildDashboardData("o", "r", issues)
	s := d.Summary

	if s.Total != 3 || s.Open != 1 || s.Closed != 2 {
		t.Fatalf("计数错误: %+v", s)
	}
	if s.CloseRate != 66.7 {
		t.Fatalf("期望关闭率 66.7，实际 %v", s.CloseRate)
	}
	if s.CloseDays == nil || *s.CloseDays != 15 {
		t.Fatalf("期望平均关闭耗时 15 天，实际 %v", s.CloseDays)
	}
	if s.LabelCount != 0 {
		t.Fatalf("期望标签数 0，实际 %d", s.LabelCount)
	}
	if s.Contributors != 1 {
		t.Fatalf("期望参与人数 1，实际 %d", s.Contributors)
	}
}

func TestBuildDashboardData_MonthlyFillsGaps(t *testing.T) {
	// 2024-01 与 2024-04 各一个，中间的 02/03 必须被补成 0，否则折线会失真
	issues := []*github.Issue{
		newIssue(1, "open", "2024-01-15", "", false),
		newIssue(2, "closed", "2024-04-15", "2024-04-20", false),
	}

	d := BuildDashboardData("o", "r", issues)

	want := []string{"2024-01", "2024-02", "2024-03", "2024-04"}
	got := make([]string, 0, len(d.Monthly))
	for _, m := range d.Monthly {
		got = append(got, m.Month)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("月份序列错误，期望 %v，实际 %v", want, got)
	}
	if d.Monthly[1].Created != 0 || d.Monthly[1].Closed != 0 {
		t.Fatalf("缺失月份应补 0，实际 %+v", d.Monthly[1])
	}
	// 1 月新建 1，4 月新建 1 并关闭 1 → 期末存量 1
	if last := d.Monthly[len(d.Monthly)-1]; last.Backlog != 1 {
		t.Fatalf("期望期末存量 1，实际 %+v", last)
	}
}

func TestBuildDashboardData_EmptyInput(t *testing.T) {
	d := BuildDashboardData("o", "r", nil)
	if d.Summary.Total != 0 || len(d.Issues) != 0 {
		t.Fatalf("空输入应产出空数据集，实际 %+v", d.Summary)
	}
	if len(d.Monthly) != 0 {
		t.Fatalf("空输入不应产出月度数据，实际 %d 条", len(d.Monthly))
	}
	// 空数据集也必须能序列化并渲染出可打开的页面
	if _, err := json.Marshal(d); err != nil {
		t.Fatalf("空数据集序列化失败: %v", err)
	}
}

// 回归：无标签 issue 的 labels 若被序列化成 null，前端 flatMap 会拿到 [null] 并抛异常，
// 进而中断整页脚本，导致表格与筛选完全不可用。
func TestIssueView_LabelsNeverNull(t *testing.T) {
	d := BuildDashboardData("o", "r", []*github.Issue{
		newIssue(1, "open", "2024-01-01", "", false), // 无标签
	})

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), `"labels":null`) {
		t.Fatalf("labels 被序列化成 null，前端会崩: %s", raw)
	}
	if !strings.Contains(string(raw), `"labels":[]`) {
		t.Fatalf("无标签 issue 的 labels 应是空数组: %s", raw)
	}
}

func TestRenderDashboard_ProducesSelfContainedPage(t *testing.T) {
	d := BuildDashboardData("o", "r", []*github.Issue{
		newIssue(1, "open", "2024-01-01", "", false, "bug"),
	})

	html, err := RenderDashboard(d)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}

	for _, marker := range []string{
		"const DATA = {", "id=\"kpis\"", "chart-status", "chart-labels",
		"chart-monthly", "chart-creators", "id=\"tbody\"", "o/r",
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("页面缺少必要标记 %q", marker)
		}
	}
	// 数据必须内嵌，不能依赖 fetch（file:// 下会被 CORS 拦）
	if strings.Contains(html, "fetch(") {
		t.Fatal("页面不应通过 fetch 取数据")
	}
	// 内嵌 JSON 必须可解析，且不能出现能提前闭合 script 的序列
	start := strings.Index(html, "const DATA = ") + len("const DATA = ")
	end := strings.Index(html[start:], ";\n") + start
	var got DashboardData
	if err := json.Unmarshal([]byte(html[start:end]), &got); err != nil {
		t.Fatalf("内嵌 JSON 无法解析: %v", err)
	}
	if got.Summary.Total != 1 {
		t.Fatalf("内嵌数据不正确: %+v", got.Summary)
	}
	if strings.Contains(html[start:end], "</script>") {
		t.Fatal("内嵌 JSON 含 </script>，会提前截断脚本")
	}
}

func TestTopN_StableOrder(t *testing.T) {
	got := topN(map[string]int{"b": 2, "a": 2, "c": 5}, 2)
	want := []NameValue{{Name: "c", Value: 5}, {Name: "a", Value: 2}}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("排序不稳定或截断错误，期望 %v，实际 %v", want, got)
	}
}

func TestSanitizeFilename_KeepsValidUTF8(t *testing.T) {
	long := strings.Repeat("中文标题", 30) // 120 个 rune，远超 50 上限
	got := sanitizeFilename(long)
	if !utf8Valid(got) {
		t.Fatalf("截断后不是合法 UTF-8: %q", got)
	}
	if runeLen(got) > 50 {
		t.Fatalf("截断后应为 50 个 rune，实际 %d", runeLen(got))
	}
	if got := sanitizeFilename("  "); got != "untitled" {
		t.Fatalf("空白标题应回落为 untitled，实际 %q", got)
	}
	if got := sanitizeFilename("a/b:c"); got != "a_b_c" {
		t.Fatalf("特殊字符未替换: %q", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == utf8.RuneError {
			return false
		}
	}
	return true
}

func runeLen(s string) int { return len([]rune(s)) }

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	if err := writeFileAtomic(target, []byte(`{"a":1}`), 0644); err != nil {
		t.Fatalf("原子写入失败: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读取结果失败: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("内容不符: %q", got)
	}

	// 写入成功后不应残留临时文件
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("目录残留了额外文件: %v", entries)
	}

	// 覆盖写
	if err := writeFileAtomic(target, []byte(`{"a":2}`), 0644); err != nil {
		t.Fatalf("覆盖写失败: %v", err)
	}
	got, _ = os.ReadFile(target)
	if string(got) != `{"a":2}` {
		t.Fatalf("覆盖后内容不符: %q", got)
	}
}

func TestNormalizeArgs(t *testing.T) {
	// 这些 flag 在 main() 中定义；测试二进制不会调用 main()，这里先注册一份
	flag.Bool("ai", false, "")
	flag.Bool("chart", false, "")
	flag.String("output", "", "")

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"flag 在位置参数之后", []string{"owner/repo", "-ai"}, []string{"-ai", "owner/repo"}},
		{"带值的 flag 在位置参数之后", []string{"owner/repo", "-output", "/tmp/x"}, []string{"-output", "/tmp/x", "owner/repo"}},
		{"bool flag 不吃后续位置参数", []string{"-ai", "owner/repo"}, []string{"-ai", "owner/repo"}},
		{"等号形式不消费后续参数", []string{"-output=/tmp/x", "owner/repo"}, []string{"-output=/tmp/x", "owner/repo"}},
		{"多个位置参数保持相对顺序", []string{"a", "-ai", "b"}, []string{"-ai", "a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeArgs(c.in)
			if !reflect.DeepEqual(c.want, got) {
				t.Fatalf("期望 %v，实际 %v", c.want, got)
			}
		})
	}
}
