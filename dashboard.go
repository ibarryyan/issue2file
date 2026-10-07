package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v57/github"
)

//go:embed templates/dashboard.html
var dashboardFS embed.FS

const (
	layoutDate     = "2006-01-02"
	layoutDateTime = "2006-01-02 15:04:05"
)

// IssueView 是单个 issue 在前端消费时的视图模型。
type IssueView struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Author    string   `json:"author"`
	Labels    []string `json:"labels"`
	Comments  int      `json:"comments"`
	CreatedAt string   `json:"createdAt"`
	ClosedAt  string   `json:"closedAt"`
	Days      *int     `json:"days"` // 已关闭=处理耗时；未关闭=存活天数
	URL       string   `json:"url"`
}

// NameValue 是通用的「名称-数值」聚合项。
type NameValue struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

// MonthlyPoint 是月度趋势上的一个点。
type MonthlyPoint struct {
	Month    string `json:"month"`
	Created  int    `json:"created"`
	Closed   int    `json:"closed"`
	Backlog  int    `json:"backlog"` // 累计存量 = 累计新建 - 累计关闭
	CumOpen  int    `json:"-"`
	CumClose int    `json:"-"`
}

// Summary 是面板顶部的 KPI 汇总。
type Summary struct {
	Total        int      `json:"total"`
	Open         int      `json:"open"`
	Closed       int      `json:"closed"`
	OpenRate     float64  `json:"openRate"`
	CloseRate    float64  `json:"closeRate"`
	CloseDays    *float64 `json:"closeDays"`   // 已关闭 issue 的平均处理天数
	OpenAgeDays  *float64 `json:"openAgeDays"` // 未关闭 issue 的平均存活天数
	Contributors int      `json:"contributors"`
	LabelCount   int      `json:"labelCount"`
}

// Meta 描述数据集的来源信息。
type Meta struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	FullName    string `json:"fullName"`
	RepoURL     string `json:"repoUrl"`
	GeneratedAt string `json:"generatedAt"`
	Source      string `json:"source"`
}

// DashboardData 是 dashboard 的完整数据集，同时落盘为 issues.json。
type DashboardData struct {
	Meta       Meta           `json:"meta"`
	Summary    Summary        `json:"summary"`
	StatusDist []NameValue    `json:"statusDist"`
	LabelDist  []NameValue    `json:"labelDist"`
	Monthly    []MonthlyPoint `json:"monthly"`
	Creators   []NameValue    `json:"creators"`
	Issues     []IssueView    `json:"issues"`
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func floatPtr(f float64) *float64 { return &f }

// BuildDashboardData 把 GitHub issue 列表聚合成面板数据集。
// 传入的 issues 允许包含 Pull Request，这里会统一过滤掉。
func BuildDashboardData(owner, repo string, issues []*github.Issue) DashboardData {
	now := time.Now()

	data := DashboardData{
		Meta: Meta{
			Owner:       owner,
			Repo:        repo,
			FullName:    owner + "/" + repo,
			RepoURL:     fmt.Sprintf("https://github.com/%s/%s", owner, repo),
			GeneratedAt: now.Format(layoutDateTime),
			Source:      "github-rest-api-v3",
		},
		Issues: []IssueView{},
	}

	statusCount := make(map[string]int)
	labelCount := make(map[string]int)
	creatorCount := make(map[string]int)

	for _, issue := range issues {
		if issue == nil || issue.IsPullRequest() {
			continue
		}

		created := issue.GetCreatedAt().Time
		state := strings.ToLower(issue.GetState())

		// 必须用 make 初始化：nil 切片会序列化成 JSON 的 null，
		// 前端再对 null 做 flatMap/join 会直接抛异常导致整页脚本中断
		labels := make([]string, 0, len(issue.Labels))
		for _, l := range issue.Labels {
			labels = append(labels, l.GetName())
		}

		view := IssueView{
			Number:    issue.GetNumber(),
			Title:     issue.GetTitle(),
			State:     state,
			Author:    issue.GetUser().GetLogin(),
			Labels:    labels,
			Comments:  issue.GetComments(),
			CreatedAt: created.Format(layoutDate),
			URL:       issue.GetHTMLURL(),
		}

		if ca := issue.ClosedAt; ca != nil && !ca.Time.IsZero() {
			view.ClosedAt = ca.Time.Format(layoutDate)
			d := int(ca.Time.Sub(created).Hours() / 24)
			view.Days = &d
		} else {
			d := int(now.Sub(created).Hours() / 24)
			view.Days = &d
		}

		data.Issues = append(data.Issues, view)

		statusCount[state]++
		for _, l := range labels {
			labelCount[l]++
		}
		if view.Author != "" {
			creatorCount[view.Author]++
		}
	}

	// 汇总指标
	s := Summary{Total: len(data.Issues)}
	for _, v := range data.Issues {
		if v.State == "open" {
			s.Open++
		} else {
			s.Closed++
		}
	}
	if s.Total > 0 {
		s.OpenRate = round1(float64(s.Open) / float64(s.Total) * 100)
		s.CloseRate = round1(float64(s.Closed) / float64(s.Total) * 100)
	}

	var closeSum, openSum float64
	var closeN, openN int
	for _, v := range data.Issues {
		if v.Days == nil {
			continue
		}
		if v.State == "open" {
			openSum += float64(*v.Days)
			openN++
		} else {
			closeSum += float64(*v.Days)
			closeN++
		}
	}
	if closeN > 0 {
		s.CloseDays = floatPtr(round1(closeSum / float64(closeN)))
	}
	if openN > 0 {
		s.OpenAgeDays = floatPtr(round1(openSum / float64(openN)))
	}
	s.Contributors = len(creatorCount)
	s.LabelCount = len(labelCount)
	data.Summary = s

	// 分布数据：按数量降序，数量相同按名称升序，保证输出稳定可复现
	data.StatusDist = topN(statusCount, 0)
	data.LabelDist = topN(labelCount, 10)
	data.Creators = topN(creatorCount, 10)
	data.Monthly = buildMonthly(data.Issues)

	return data
}

// topN 把计数 map 转成排序后的切片，n<=0 表示全量。
func topN(m map[string]int, n int) []NameValue {
	out := make([]NameValue, 0, len(m))
	for k, v := range m {
		out = append(out, NameValue{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return out[i].Name < out[j].Name
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// buildMonthly 生成按月聚合的新建/关闭趋势，并补齐中间没有数据的月份。
func buildMonthly(issues []IssueView) []MonthlyPoint {
	if len(issues) == 0 {
		return []MonthlyPoint{}
	}

	createdByMonth := make(map[string]int)
	closedByMonth := make(map[string]int)
	min, max := time.Now(), time.Time{}

	for _, it := range issues {
		t, err := time.Parse(layoutDate, it.CreatedAt)
		if err != nil {
			continue
		}
		key := t.Format("2006-01")
		createdByMonth[key]++
		if t.Before(min) {
			min = t
		}
		if t.After(max) {
			max = t
		}
		if it.ClosedAt != "" {
			if ct, err := time.Parse(layoutDate, it.ClosedAt); err == nil {
				closedByMonth[ct.Format("2006-01")]++
				if ct.After(max) {
					max = ct
				}
			}
		}
	}

	start := time.Date(min.Year(), min.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(max.Year(), max.Month(), 1, 0, 0, 0, 0, time.UTC)

	out := make([]MonthlyPoint, 0, 64)
	cumCreated, cumClosed := 0, 0
	for cur := start; !cur.After(end); cur = cur.AddDate(0, 1, 0) {
		key := cur.Format("2006-01")
		c := createdByMonth[key]
		d := closedByMonth[key]
		cumCreated += c
		cumClosed += d
		out = append(out, MonthlyPoint{
			Month:   key,
			Created: c,
			Closed:  d,
			Backlog: cumCreated - cumClosed,
		})
	}
	return out
}

type dashboardPage struct {
	Title       string
	RepoURL     string
	GeneratedAt string
	Data        template.JS
}

// GenerateDashboard 生成 issues.json 数据集与 dashboard.html 面板。
func GenerateDashboard(owner, repo, outputDir string, issues []*github.Issue) (string, error) {
	data := BuildDashboardData(owner, repo, issues)

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化面板数据失败: %w", err)
	}

	// 数据集单独落盘，方便被其他工具消费或前端二次渲染
	dataPath := filepath.Join(outputDir, "issues.json")
	if err := writeFileAtomic(dataPath, raw, 0644); err != nil {
		return "", fmt.Errorf("写入 %s 失败: %w", dataPath, err)
	}

	html, err := RenderDashboard(data)
	if err != nil {
		return "", err
	}

	pagePath := filepath.Join(outputDir, "dashboard.html")
	if err := writeFileAtomic(pagePath, []byte(html), 0644); err != nil {
		return "", fmt.Errorf("写入 %s 失败: %w", pagePath, err)
	}

	return pagePath, nil
}

// RenderDashboard 把数据集渲染成单文件 HTML，数据内嵌，
// 这样通过 file:// 直接双击打开也不会被 CORS 拦掉 fetch。
func RenderDashboard(data DashboardData) (string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("序列化面板数据失败: %w", err)
	}

	tmpl, err := template.ParseFS(dashboardFS, "templates/dashboard.html")
	if err != nil {
		return "", fmt.Errorf("解析面板模板失败: %w", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, dashboardPage{
		Title:       data.Meta.FullName,
		RepoURL:     data.Meta.RepoURL,
		GeneratedAt: data.Meta.GeneratedAt,
		Data:        template.JS(raw),
	}); err != nil {
		return "", fmt.Errorf("渲染面板失败: %w", err)
	}
	return buf.String(), nil
}

// writeFileAtomic 通过「临时文件 + 同目录 rename」保证写盘的原子性，
// 避免进程被中断时在输出目录留下半截文件。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".i2f-tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 任何失败路径都清理临时文件

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	// Close 的错误必须检查，否则可能并未真正落盘
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("重命名到 %s 失败: %w", path, err)
	}
	return nil
}
