package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/components"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/go-echarts/go-echarts/v2/types"
	"github.com/google/go-github/v57/github"
)

// 生成所有图表
func generateCharts(issues []*github.Issue, outputDirPath string) error {
	// 创建图表目录
	chartsDir := filepath.Join(outputDirPath, "charts")
	if err := os.MkdirAll(chartsDir, 0755); err != nil {
		return fmt.Errorf("创建图表目录失败: %w", err)
	}

	// 生成状态分布图
	if err := generateStatusChart(issues, chartsDir); err != nil {
		return fmt.Errorf("生成状态分布图失败: %w", err)
	}

	// 生成标签分布图
	if err := generateLabelsChart(issues, chartsDir); err != nil {
		return fmt.Errorf("生成标签分布图失败: %w", err)
	}

	// 生成时间趋势图
	if err := generateTimelineChart(issues, chartsDir); err != nil {
		return fmt.Errorf("生成时间趋势图失败: %w", err)
	}

	// 生成图表索引页
	if err := generateChartsIndex(chartsDir); err != nil {
		return fmt.Errorf("生成图表索引页失败: %w", err)
	}

	return nil
}

// 生成状态分布图
func generateStatusChart(issues []*github.Issue, chartsDir string) error {
	// 统计不同状态的issue数量
	statusCount := make(map[string]int)
	for _, issue := range issues {
		status := issue.GetState()
		statusCount[status]++
	}

	// 创建饼图实例
	pie := charts.NewPie()
	pie.SetGlobalOptions(
		charts.WithInitializationOpts(opts.Initialization{
			Theme:  types.ThemeWesteros,
			Width:  "800px",
			Height: "600px",
		}),
		charts.WithTitleOpts(opts.Title{
			Title:    "Issues状态分布",
			Subtitle: fmt.Sprintf("总数: %d", len(issues)),
		}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true)}),
		charts.WithLegendOpts(opts.Legend{Show: opts.Bool(true)}),
	)

	// 准备数据
	items := make([]opts.PieData, 0, len(statusCount))
	for status, count := range statusCount {
		items = append(items, opts.PieData{
			Name:  status,
			Value: count,
		})
	}

	// 添加数据到图表
	pie.AddSeries("状态", items).
		SetSeriesOptions(
			charts.WithLabelOpts(opts.Label{
				Show:      opts.Bool(true),
				Formatter: "{b}: {c} ({d}%)",
			}),
			charts.WithPieChartOpts(opts.PieChart{
				Radius: []string{"40%", "70%"},
			}),
		)

	// 保存图表
	f, err := os.Create(filepath.Join(chartsDir, "status_chart.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return pie.Render(f)
}

// 生成标签分布图
func generateLabelsChart(issues []*github.Issue, chartsDir string) error {
	// 统计不同标签的issue数量
	labelCount := make(map[string]int)
	for _, issue := range issues {
		if len(issue.Labels) == 0 {
			labelCount["无标签"]++
			continue
		}

		for _, label := range issue.Labels {
			labelName := label.GetName()
			labelCount[labelName]++
		}
	}

	// 按数量排序标签
	type labelItem struct {
		Name  string
		Count int
	}
	var labelItems []labelItem
	for name, count := range labelCount {
		labelItems = append(labelItems, labelItem{Name: name, Count: count})
	}
	sort.Slice(labelItems, func(i, j int) bool {
		return labelItems[i].Count > labelItems[j].Count
	})

	// 如果标签太多，只取前10个
	maxLabels := 10
	if len(labelItems) > maxLabels {
		labelItems = labelItems[:maxLabels]
	}

	// 创建柱状图实例
	bar := charts.NewBar()
	bar.SetGlobalOptions(
		charts.WithInitializationOpts(opts.Initialization{
			Theme:  types.ThemeWesteros,
			Width:  "900px",
			Height: "500px",
		}),
		charts.WithTitleOpts(opts.Title{
			Title:    "Issues标签分布",
			Subtitle: fmt.Sprintf("前%d个标签", len(labelItems)),
		}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true)}),
		charts.WithLegendOpts(opts.Legend{Show: opts.Bool(false)}),
		charts.WithXAxisOpts(opts.XAxis{
			Name:      "标签",
			AxisLabel: &opts.AxisLabel{Rotate: 45},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Name: "数量",
		}),
	)

	// 准备数据
	xAxis := make([]string, 0, len(labelItems))
	values := make([]opts.BarData, 0, len(labelItems))
	for _, item := range labelItems {
		xAxis = append(xAxis, item.Name)
		values = append(values, opts.BarData{Value: item.Count})
	}

	// 添加数据到图表
	bar.SetXAxis(xAxis).
		AddSeries("数量", values).
		SetSeriesOptions(
			charts.WithLabelOpts(opts.Label{
				Show:     opts.Bool(true),
				Position: "top",
			}),
		)

	// 保存图表
	f, err := os.Create(filepath.Join(chartsDir, "labels_chart.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return bar.Render(f)
}

// 生成时间趋势图
func generateTimelineChart(issues []*github.Issue, chartsDir string) error {
	// 按月统计issue创建数量
	monthlyCount := make(map[string]int)

	// 找出最早和最晚的日期
	var earliestDate, latestDate time.Time
	for i, issue := range issues {
		createdAt := issue.GetCreatedAt()
		if i == 0 || createdAt.Before(earliestDate) {
			earliestDate = createdAt.Time
		}
		if i == 0 || createdAt.After(latestDate) {
			latestDate = createdAt.Time
		}
	}

	// 生成所有月份的键
	current := time.Date(earliestDate.Year(), earliestDate.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(latestDate.Year(), latestDate.Month(), 1, 0, 0, 0, 0, time.UTC)
	for !current.After(end) {
		monthKey := current.Format("2006-01")
		monthlyCount[monthKey] = 0
		current = current.AddDate(0, 1, 0)
	}

	// 统计每月的issue数量
	for _, issue := range issues {
		monthKey := issue.GetCreatedAt().Format("2006-01")
		monthlyCount[monthKey]++
	}

	// 按时间顺序排序月份
	var months []string
	for month := range monthlyCount {
		months = append(months, month)
	}
	sort.Strings(months)

	// 创建折线图实例
	line := charts.NewLine()
	line.SetGlobalOptions(
		charts.WithInitializationOpts(opts.Initialization{
			Theme:  types.ThemeWesteros,
			Width:  "1000px",
			Height: "500px",
		}),
		charts.WithTitleOpts(opts.Title{
			Title:    "Issues创建时间趋势",
			Subtitle: fmt.Sprintf("从 %s 到 %s", earliestDate.Format("2006-01"), latestDate.Format("2006-01")),
		}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true)}),
		charts.WithLegendOpts(opts.Legend{Show: opts.Bool(true)}),
		charts.WithXAxisOpts(opts.XAxis{
			Name:      "月份",
			AxisLabel: &opts.AxisLabel{Rotate: 45},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Name: "数量",
		}),
		charts.WithDataZoomOpts(opts.DataZoom{
			Type:  "inside",
			Start: 0,
			End:   100,
		}),
		charts.WithDataZoomOpts(opts.DataZoom{
			Type:  "slider",
			Start: 0,
			End:   100,
		}),
	)

	// 准备数据
	values := make([]opts.LineData, 0, len(months))
	for _, month := range months {
		values = append(values, opts.LineData{Value: monthlyCount[month]})
	}

	// 添加数据到图表
	line.SetXAxis(months).
		AddSeries("新建Issues", values).
		SetSeriesOptions(
			charts.WithLineChartOpts(opts.LineChart{
				Smooth: opts.Bool(true),
			}),
			charts.WithLabelOpts(opts.Label{
				Show: opts.Bool(true),
			}),
			charts.WithMarkPointNameTypeItemOpts(
				opts.MarkPointNameTypeItem{Name: "最大值", Type: "max"},
				opts.MarkPointNameTypeItem{Name: "最小值", Type: "min"},
			),
			charts.WithMarkLineNameTypeItemOpts(
				opts.MarkLineNameTypeItem{Name: "平均值", Type: "average"},
			),
		)

	// 保存图表
	f, err := os.Create(filepath.Join(chartsDir, "timeline_chart.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return line.Render(f)
}

// 生成图表索引页
func generateChartsIndex(chartsDir string) error {
	page := components.NewPage()
	page.SetLayout(components.PageFlexLayout)

	// 创建HTML内容
	content := `
    <div style='margin: 20px; text-align: center;'>
        <h1>GitHub Issues 图表分析</h1>
        <div style="display: flex; flex-direction: column; gap: 15px; margin-top: 30px;">
            <a href="status_chart.html" style="font-size: 18px; padding: 10px; background-color: #f0f0f0; border-radius: 5px; text-decoration: none; color: #333;">状态分布图</a>
            <a href="labels_chart.html" style="font-size: 18px; padding: 10px; background-color: #f0f0f0; border-radius: 5px; text-decoration: none; color: #333;">标签分布图</a>
            <a href="timeline_chart.html" style="font-size: 18px; padding: 10px; background-color: #f0f0f0; border-radius: 5px; text-decoration: none; color: #333;">时间趋势图</a>
        </div>
    </div>
    `

	// 使用自定义HTML内容
	custom := charts.NewCustom()
	custom.AddCustomizedHeaders(content)
	page.AddCharts(custom)
	page.SetPageTitle("GitHub Issues 图表分析")

	// 保存索引页
	f, err := os.Create(filepath.Join(chartsDir, "index.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return page.Render(f)
}
