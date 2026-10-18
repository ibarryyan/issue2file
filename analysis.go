package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/go-github/v57/github"
)

// issue 分类常量。分类结果会直接展示在页面上，取值必须稳定。
const (
	catBug      = "缺陷"
	catFeature  = "需求"
	catQuestion = "咨询"
	catDocs     = "文档"
	catChore    = "工程"
	catOther    = "其他"
)

// 分类规则按优先级排列：命中分数相同时，靠前的规则胜出。
// 缺陷排在最前，因为「build failure」这类标题同时命中工程与缺陷，
// 从维护视角看它更应该被当成缺陷处理。
type categoryRule struct {
	name     string
	keywords []string
}

var categoryRules = []categoryRule{
	{catBug, []string{
		"bug", "fix", "error", "exception", "crash", "panic", "fail", "failure", "broken",
		"regression", "defect", "traceback", "segfault", "leak", "deadlock", "stack trace",
		"报错", "错误", "异常", "崩溃", "失败", "缺陷", "无法", "不能", "复现", "闪退", "卡死", "漏洞", "不生效", "无效",
	}},
	{catFeature, []string{
		"feature", "enhancement", "proposal", "request", "rfc", "improve", "support", "wish",
		"新增", "增加", "添加", "希望", "希望可以", "建议", "支持", "优化", "增强", "功能", "需求",
		"能否", "期待", "补齐", "缺少",
	}},
	{catQuestion, []string{
		"question", "help wanted", "how to", "how do", "discussion", "ask",
		"求助", "咨询", "请问", "如何", "怎么", "为什么", "疑问", "不解",
	}},
	{catDocs, []string{
		"docs", "documentation", "readme", "typo", "changelog",
		"文档", "示例", "注释", "拼写", "说明",
	}},
	{catChore, []string{
		"ci", "build", "deps", "dependency", "dependencies", "refactor", "chore", "release",
		"bump", "upgrade", "pipeline", "workflow",
		"构建", "依赖", "升级", "重构", "发布", "流水线",
	}},
}

// 紧急信号：命中即大幅提升优先级。
var urgentKeywords = []string{
	"urgent", "critical", "blocker", "p0", "severe", "security", "cve", "data loss", "outage",
	"紧急", "严重", "阻塞", "阻断", "线上", "生产环境", "安全", "数据丢失", "不可用",
}

var (
	// 英文词：至少 3 个字符，避免 "to"、"in" 之类的噪声
	wordRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_+#.\-]{2,}`)
	// 连续汉字片段，用于生成 2~3 字词组
	cjkRunRe = regexp.MustCompile(`[\p{Han}]{2,}`)
	// 堆栈/日志特征
	stackRe = regexp.MustCompile(`(?i)(goroutine\s+\d+|traceback \(most recent|stack ?trace|panic:|at\s+[\w.$]+\([\w.]+\.(java|kt|go|py|js|ts):\d+|堆栈|调用栈)`)
	// Markdown 代码围栏
	fenceRe = regexp.MustCompile("```")
)

const (
	bodyScanLimit    = 4000 // 分类时正文最多扫描的字符数（按字节截断，够用即可）
	keywordScanLimit = 6000 // 关键词抽取时正文最多扫描的字符数
)

var stopWordsEn = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "this": true, "with": true, "from": true,
	"have": true, "has": true, "not": true, "but": true, "are": true, "was": true, "were": true,
	"when": true, "where": true, "what": true, "which": true, "who": true, "how": true, "why": true,
	"can": true, "could": true, "should": true, "would": true, "will": true, "shall": true,
	"you": true, "your": true, "our": true, "their": true, "its": true, "it's": true, "they": true,
	"there": true, "here": true, "then": true, "than": true, "into": true, "out": true, "about": true,
	"issue": true, "issues": true, "please": true, "thanks": true, "thank": true, "hello": true,
	"any": true, "all": true, "some": true, "more": true, "most": true, "other": true, "such": true,
	"only": true, "just": true, "also": true, "very": true, "much": true, "many": true, "get": true,
	"got": true, "use": true, "used": true, "using": true, "make": true, "made": true, "like": true,
	"want": true, "need": true, "know": true, "think": true, "see": true, "seem": true, "one": true,
	"two": true, "now": true, "new": true, "old": true, "way": true, "thing": true, "things": true,
	"my": true, "me": true, "we": true, "us": true, "if": true, "so": true, "as": true, "at": true,
	"by": true, "on": true, "in": true, "to": true, "of": true, "or": true, "no": true, "yes": true,
	"http": true, "https": true, "com": true, "www": true, "github": true, "git": true,
}

// 中文常见虚词/高频口语词，抽关键词时过滤掉，否则全是「这个」「可以」
var stopWordsZh = map[string]bool{
	"这个": true, "那个": true, "我们": true, "他们": true, "你们": true, "可以": true, "什么": true,
	"怎么": true, "如何": true, "但是": true, "因为": true, "所以": true, "如果": true, "一下": true,
	"一个": true, "没有": true, "就是": true, "已经": true, "需要": true, "应该": true, "时候": true,
	"现在": true, "目前": true, "使用": true, "进行": true, "出现": true, "导致": true, "然后": true,
	"而且": true, "还有": true, "之后": true, "之前": true, "其中": true, "有些": true, "一些": true,
	"这样": true, "那样": true, "这里": true, "那里": true, "不会": true, "不能": true, "可能": true,
	"感觉": true, "希望": true, "谢谢": true, "问题": true, "情况": true, "相关": true, "其实": true,
	"以及": true, "或者": true, "不过": true, "虽然": true, "例如": true, "比如": true, "通过": true,
	"两个": true, "三种": true, "多个": true, "第二": true, "第一": true, "开始": true, "结束": true,
}

// issueAnalysis 是单个 issue 的深度分析结果。
type issueAnalysis struct {
	Number       int      `json:"number"`
	Title        string   `json:"title"`
	URL          string   `json:"url"`
	State        string   `json:"state"`
	Author       string   `json:"author"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Milestone    string   `json:"milestone"`
	CreatedAt    string   `json:"createdAt"`
	ClosedAt     string   `json:"closedAt"`
	Days         int      `json:"days"`
	Comments     int      `json:"comments"`
	Participants int      `json:"participants"`
	Words        int      `json:"words"`
	CodeBlocks   int      `json:"codeBlocks"`
	HasStack     bool     `json:"hasStack"`
	Category     string   `json:"category"`
	Priority     string   `json:"priority"`
	Score        int      `json:"score"`
	Reasons      []string `json:"reasons"`
	Keywords     []string `json:"keywords"`
	Summary      string   `json:"summary"`
	AIInsight    string   `json:"aiInsight"`
}

// classifyIssue 依据标题、标签、正文关键词给 issue 归类，并返回命中的关键词。
func classifyIssue(title string, labels []string, body string) (string, []string) {
	titleL := strings.ToLower(title)
	labelL := strings.ToLower(strings.Join(labels, " "))
	if len(body) > bodyScanLimit {
		body = body[:bodyScanLimit]
	}
	bodyL := strings.ToLower(body)

	best := catOther
	bestScore := 0
	var hits []string

	for _, rule := range categoryRules {
		score := 0
		var ruleHits []string
		for _, kw := range rule.keywords {
			switch {
			case strings.Contains(titleL, kw):
				score += 3
			case strings.Contains(labelL, kw):
				score += 2
			case strings.Contains(bodyL, kw):
				score += 1
			default:
				continue
			}
			ruleHits = append(ruleHits, kw)
		}
		if score > bestScore {
			bestScore, best, hits = score, rule.name, ruleHits
		}
	}
	return best, hits
}

// priorityOf 综合状态、热度、类型、紧急信号、积压时长给出优先级与依据。
func priorityOf(state string, days, comments int, category, title, body string, hasAssignee, hasStack bool) (string, int, []string) {
	score := 0
	var reasons []string

	if strings.EqualFold(state, "open") {
		score += 2
		reasons = append(reasons, "仍处于未关闭状态")
	}
	switch {
	case comments >= 10:
		score += 2
		reasons = append(reasons, fmt.Sprintf("评论 %d 条，讨论热度高", comments))
	case comments >= 3:
		score += 1
		reasons = append(reasons, fmt.Sprintf("评论 %d 条，有一定讨论", comments))
	}
	if category == catBug {
		score += 2
		reasons = append(reasons, "归类为缺陷，影响可用性")
	}
	if hasUrgentSignal(title, body) {
		score += 3
		reasons = append(reasons, "标题/正文含紧急或安全相关信号")
	}
	if strings.EqualFold(state, "open") {
		switch {
		case days >= 365:
			score += 2
			reasons = append(reasons, fmt.Sprintf("已积压 %d 天", days))
		case days >= 180:
			score += 1
			reasons = append(reasons, fmt.Sprintf("已积压 %d 天", days))
		}
		if !hasAssignee {
			score += 1
			reasons = append(reasons, "无人认领（无指派人）")
		}
	}
	if hasStack {
		score += 1
		reasons = append(reasons, "附带堆栈/日志，信息较完整")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "暂无明显风险信号")
	}

	var level string
	switch {
	case score >= 7:
		level = "P0"
	case score >= 5:
		level = "P1"
	case score >= 3:
		level = "P2"
	default:
		level = "P3"
	}
	return level, score, reasons
}

func hasUrgentSignal(title, body string) bool {
	hay := strings.ToLower(title)
	if len(body) > bodyScanLimit {
		body = body[:bodyScanLimit]
	}
	hay += " " + strings.ToLower(body)
	for _, kw := range urgentKeywords {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

// extractKeywords 从标题与正文中抽取中英文关键词。
// 中文没有空格分词，这里用 2~3 字滑窗 n-gram + 频次打分，
// 再按「更长更优先」做去重，避免出现「配置」和「配置项」同时上榜。
func extractKeywords(title, body string, limit int) []string {
	if limit <= 0 {
		limit = 8
	}
	if len(body) > keywordScanLimit {
		body = body[:keywordScanLimit]
	}
	text := title + " " + body

	count := make(map[string]int)
	for _, w := range wordRe.FindAllString(strings.ToLower(text), -1) {
		w = strings.Trim(w, ".-_+#")
		if len(w) < 3 || stopWordsEn[w] {
			continue
		}
		count[w]++
	}

	titleRuns := cjkRunRe.FindAllString(title, -1)
	inTitle := make(map[string]bool)
	for _, run := range titleRuns {
		rs := []rune(run)
		for n := 2; n <= 3; n++ {
			for i := 0; i+n <= len(rs); i++ {
				inTitle[string(rs[i:i+n])] = true
			}
		}
	}
	for _, run := range cjkRunRe.FindAllString(text, -1) {
		rs := []rune(run)
		for n := 2; n <= 3; n++ {
			for i := 0; i+n <= len(rs); i++ {
				g := string(rs[i : i+n])
				if stopWordsZh[g] {
					continue
				}
				count[g]++
			}
		}
	}

	type cand struct {
		term string
		freq int
	}
	cands := make([]cand, 0, len(count))
	for t, f := range count {
		// 只出现一次的词组噪声太大，除非它出现在标题里
		if f < 2 && !inTitle[t] {
			continue
		}
		cands = append(cands, cand{t, f})
	}
	sort.Slice(cands, func(i, j int) bool {
		si := cands[i].freq * utf8.RuneCountInString(cands[i].term)
		sj := cands[j].freq * utf8.RuneCountInString(cands[j].term)
		if si != sj {
			return si > sj
		}
		if cands[i].freq != cands[j].freq {
			return cands[i].freq > cands[j].freq
		}
		li := utf8.RuneCountInString(cands[i].term)
		lj := utf8.RuneCountInString(cands[j].term)
		if li != lj {
			return li > lj
		}
		return cands[i].term < cands[j].term
	})

	out := make([]string, 0, limit)
	for _, c := range cands {
		dup := false
		for _, picked := range out {
			if strings.Contains(picked, c.term) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, c.term)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// countWords 粗略统计正文规模：英文按词数，中文按汉字数，两者相加。
func countWords(body string) int {
	words := len(strings.Fields(body))
	han := 0
	for _, r := range body {
		if unicode.Is(unicode.Han, r) {
			han++
		}
	}
	return words + han
}

// deepAnalyze 对选中的 issue 做逐项深度分析，并产出四组聚合分布。
func deepAnalyze(issues []*github.Issue, now time.Time, comments map[int][]*github.IssueComment) (
	[]issueAnalysis, []NameValue, []NameValue, []NameValue, []NameValue) {

	items := make([]issueAnalysis, 0, len(issues))
	catCount := make(map[string]int)
	prioCount := make(map[string]int)
	kwCount := make(map[string]int)
	ageCount := make(map[string]int)

	for _, issue := range issues {
		if issue == nil {
			continue
		}
		title := issue.GetTitle()
		body := issue.GetBody()
		state := strings.ToLower(issue.GetState())

		labels := make([]string, 0, len(issue.Labels))
		for _, l := range issue.Labels {
			labels = append(labels, l.GetName())
		}
		assignees := make([]string, 0, len(issue.Assignees))
		for _, a := range issue.Assignees {
			assignees = append(assignees, a.GetLogin())
		}

		created := issue.GetCreatedAt().Time
		var days int
		var closedAt string
		if ca := issue.ClosedAt; ca != nil && !ca.Time.IsZero() {
			closedAt = ca.Time.Format(layoutDate)
			days = int(ca.Time.Sub(created).Hours() / 24)
		} else {
			days = int(now.Sub(created).Hours() / 24)
		}

		cms := comments[issue.GetNumber()]
		commentN := issue.GetComments()
		if len(cms) > commentN {
			commentN = len(cms)
		}
		people := map[string]bool{issue.GetUser().GetLogin(): true}
		for _, c := range cms {
			if login := c.GetUser().GetLogin(); login != "" {
				people[login] = true
			}
		}
		delete(people, "")

		category := catOther
		if len(cms) > 0 {
			// 评论里出现的分类信号也算数，能纠正正文写得太简略的 issue
			extra := ""
			for _, c := range cms {
				extra += " " + c.GetBody()
			}
			category, _ = classifyIssue(title, labels, body+" "+extra)
		} else {
			category, _ = classifyIssue(title, labels, body)
		}

		hasStack := stackRe.MatchString(body)
		codeBlocks := len(fenceRe.FindAllString(body, -1)) / 2
		prio, score, reasons := priorityOf(state, days, commentN, category, title, body, len(assignees) > 0, hasStack)
		keywords := extractKeywords(title, body, 8)

		item := issueAnalysis{
			Number:       issue.GetNumber(),
			Title:        title,
			URL:          issue.GetHTMLURL(),
			State:        state,
			Author:       issue.GetUser().GetLogin(),
			Labels:       labels,
			Assignees:    assignees,
			Milestone:    issue.Milestone.GetTitle(),
			CreatedAt:    created.Format(layoutDate),
			ClosedAt:     closedAt,
			Days:         days,
			Comments:     commentN,
			Participants: len(people),
			Words:        countWords(body),
			CodeBlocks:   codeBlocks,
			HasStack:     hasStack,
			Category:     category,
			Priority:     prio,
			Score:        score,
			Reasons:      reasons,
			Keywords:     keywords,
			Summary:      "",
			AIInsight:    "",
		}
		item.Summary = summarizeItem(item)
		items = append(items, item)

		catCount[category]++
		prioCount[prio]++
		ageCount[ageBucket(state, days)]++
		for _, k := range keywords {
			kwCount[k]++
		}
	}

	catDist := topN(catCount, 0)
	prioDist := orderPriority(prioCount)
	kwDist := topN(kwCount, 12)
	ageDist := orderAgeBuckets(ageCount)

	return items, catDist, prioDist, kwDist, ageDist
}

// summarizeItem 生成一句话结论，便于在卡片与报告里直接展示。
func summarizeItem(it issueAnalysis) string {
	stateText := "未关闭"
	if it.State == "closed" {
		stateText = fmt.Sprintf("已关闭（耗时 %d 天）", it.Days)
	} else {
		stateText = fmt.Sprintf("未关闭（已存活 %d 天）", it.Days)
	}
	return fmt.Sprintf("#%d 「%s」：%s，分类=%s，优先级=%s（%d 分），评论 %d 条、参与 %d 人。",
		it.Number, it.Title, stateText, it.Category, it.Priority, it.Score, it.Comments, it.Participants)
}

var priorityOrder = []string{"P0", "P1", "P2", "P3"}
var ageBucketOrder = []string{"未关闭", "1 天内", "1-7 天", "7-30 天", "30-90 天", "超过 90 天"}

func orderPriority(m map[string]int) []NameValue {
	out := make([]NameValue, 0, len(priorityOrder))
	for _, p := range priorityOrder {
		if v, ok := m[p]; ok && v > 0 {
			out = append(out, NameValue{Name: p, Value: v})
		}
	}
	return out
}

func orderAgeBuckets(m map[string]int) []NameValue {
	out := make([]NameValue, 0, len(ageBucketOrder))
	for _, b := range ageBucketOrder {
		if v, ok := m[b]; ok && v > 0 {
			out = append(out, NameValue{Name: b, Value: v})
		}
	}
	return out
}

func ageBucket(state string, days int) string {
	if strings.EqualFold(state, "open") {
		return "未关闭"
	}
	switch {
	case days <= 1:
		return "1 天内"
	case days <= 7:
		return "1-7 天"
	case days <= 30:
		return "7-30 天"
	case days <= 90:
		return "30-90 天"
	default:
		return "超过 90 天"
	}
}

// BuildOverviewReport 生成「拉取全部 issue 后的初步分析报告」。
// 全部基于本地统计，不依赖 AI，保证任何仓库都能立刻产出结论。
func BuildOverviewReport(data DashboardData, repo repoInfo) string {
	var b strings.Builder

	desc := strings.TrimSpace(repo.Description)
	desc = strings.ReplaceAll(desc, "\n", " ")
	desc = strings.ReplaceAll(desc, "\r", " ")

	b.WriteString("# GitHub Issues 初步分析报告\n\n")
	b.WriteString(fmt.Sprintf("> 仓库：**%s** ｜ 生成时间：%s ｜ 数据来源：%s\n",
		data.Meta.FullName, data.Meta.GeneratedAt, data.Meta.Source))
	if desc != "" {
		b.WriteString(fmt.Sprintf("> 简介：%s\n", desc))
	}

	s := data.Summary
	b.WriteString("\n## 一、核心指标\n\n")
	b.WriteString("| 指标 | 数值 |\n|---|---|\n")
	b.WriteString(fmt.Sprintf("| issue 总数 | %d |\n", s.Total))
	b.WriteString(fmt.Sprintf("| 未关闭 / 已关闭 | %d / %d |\n", s.Open, s.Closed))
	b.WriteString(fmt.Sprintf("| 关闭率 | %.1f%% |\n", s.CloseRate))
	b.WriteString(fmt.Sprintf("| 未关闭占比 | %.1f%% |\n", s.OpenRate))
	b.WriteString(fmt.Sprintf("| 平均关闭耗时 | %s |\n", daysText(s.CloseDays)))
	b.WriteString(fmt.Sprintf("| 关闭耗时中位数 | %s |\n", daysText(floatPtrOrNil(medianCloseDays(data.Issues)))))
	b.WriteString(fmt.Sprintf("| 未关闭平均存活 | %s |\n", daysText(s.OpenAgeDays)))
	b.WriteString(fmt.Sprintf("| 参与人数 | %d |\n", s.Contributors))
	b.WriteString(fmt.Sprintf("| 使用到的标签数 | %d |\n", s.LabelCount))
	if len(data.Monthly) > 0 {
		first, last := data.Monthly[0], data.Monthly[len(data.Monthly)-1]
		b.WriteString(fmt.Sprintf("| 时间跨度 | %s ~ %s（共 %d 个月） |\n", first.Month, last.Month, len(data.Monthly)))
	}

	b.WriteString("\n## 二、状态分布\n\n")
	if len(data.StatusDist) == 0 {
		b.WriteString("暂无数据。\n")
	} else {
		b.WriteString("| 状态 | 数量 | 占比 |\n|---|---|---|\n")
		for _, it := range data.StatusDist {
			b.WriteString(fmt.Sprintf("| %s | %d | %.1f%% |\n", it.Name, it.Value, pct(it.Value, s.Total)))
		}
	}

	b.WriteString("\n## 三、标签 TOP 10\n\n")
	if len(data.LabelDist) == 0 {
		b.WriteString("该仓库的 issue 尚未使用标签。\n")
	} else {
		b.WriteString("| 标签 | 数量 | 占比 |\n|---|---|---|\n")
		for _, it := range data.LabelDist {
			b.WriteString(fmt.Sprintf("| `%s` | %d | %.1f%% |\n", it.Name, it.Value, pct(it.Value, s.Total)))
		}
	}

	b.WriteString("\n## 四、月度趋势\n\n")
	if len(data.Monthly) == 0 {
		b.WriteString("暂无数据。\n")
	} else {
		peak := data.Monthly[0]
		for _, m := range data.Monthly {
			if m.Created > peak.Created {
				peak = m
			}
		}
		last := data.Monthly[len(data.Monthly)-1]
		b.WriteString(fmt.Sprintf("- 新建量峰值：**%s**（%d 个）\n", peak.Month, peak.Created))
		b.WriteString(fmt.Sprintf("- 期末存量（累计新建 - 累计关闭）：%d\n", last.Backlog))
		b.WriteString(fmt.Sprintf("- 最近 3 个月月均新建：%.1f 个\n", recentAvg(data.Monthly, 3)))
		b.WriteString("\n| 月份 | 新建 | 关闭 | 存量 |\n|---|---|---|---|\n")
		start := 0
		if len(data.Monthly) > 12 {
			start = len(data.Monthly) - 12
		}
		for _, m := range data.Monthly[start:] {
			b.WriteString(fmt.Sprintf("| %s | %d | %d | %d |\n", m.Month, m.Created, m.Closed, m.Backlog))
		}
	}

	b.WriteString("\n## 五、活跃创建者 TOP 10\n\n")
	if len(data.Creators) == 0 {
		b.WriteString("暂无数据。\n")
	} else {
		b.WriteString("| 创建者 | issue 数 | 占比 |\n|---|---|---|\n")
		for _, it := range data.Creators {
			b.WriteString(fmt.Sprintf("| @%s | %d | %.1f%% |\n", it.Name, it.Value, pct(it.Value, s.Total)))
		}
	}

	// 六、需要关注的项目
	openIssues := make([]IssueView, 0, len(data.Issues))
	noLabel := 0
	staleOpen := 0
	for _, it := range data.Issues {
		if it.State == "open" {
			openIssues = append(openIssues, it)
			if it.Days != nil && *it.Days > 180 {
				staleOpen++
			}
		}
		if len(it.Labels) == 0 {
			noLabel++
		}
	}
	sort.SliceStable(openIssues, func(i, j int) bool {
		return derefInt(openIssues[i].Days) > derefInt(openIssues[j].Days)
	})

	b.WriteString("\n## 六、需要重点关注\n\n")
	b.WriteString("### 6.1 积压最久的未关闭 issue（TOP 5）\n\n")
	if len(openIssues) == 0 {
		b.WriteString("没有未关闭的 issue，仓库状态健康。\n")
	} else {
		b.WriteString("| 编号 | 标题 | 存活天数 | 评论 | 标签 |\n|---|---|---|---|---|\n")
		for i, it := range openIssues {
			if i >= 5 {
				break
			}
			b.WriteString(fmt.Sprintf("| [#%d](%s) | %s | %d | %d | %s |\n",
				it.Number, it.URL, escapeTableCell(it.Title), derefInt(it.Days), it.Comments, labelCell(it.Labels)))
		}
	}

	byComments := append([]IssueView(nil), data.Issues...)
	sort.SliceStable(byComments, func(i, j int) bool {
		if byComments[i].Comments != byComments[j].Comments {
			return byComments[i].Comments > byComments[j].Comments
		}
		return byComments[i].Number < byComments[j].Number
	})
	b.WriteString("\n### 6.2 讨论最热烈的 issue（TOP 5）\n\n")
	if len(byComments) == 0 {
		b.WriteString("暂无数据。\n")
	} else {
		b.WriteString("| 编号 | 标题 | 评论 | 状态 |\n|---|---|---|---|\n")
		for i, it := range byComments {
			if i >= 5 {
				break
			}
			b.WriteString(fmt.Sprintf("| [#%d](%s) | %s | %d | %s |\n",
				it.Number, it.URL, escapeTableCell(it.Title), it.Comments, it.State))
		}
	}

	b.WriteString("\n### 6.3 结构性风险\n\n")
	b.WriteString(fmt.Sprintf("- 无标签 issue：%d 个（占 %.1f%%）\n", noLabel, pct(noLabel, s.Total)))
	b.WriteString(fmt.Sprintf("- 未关闭且存活超过 180 天：%d 个\n", staleOpen))
	if s.CloseDays != nil {
		b.WriteString(fmt.Sprintf("- 平均关闭耗时 %.1f 天，%s\n", *s.CloseDays, closeSpeedComment(*s.CloseDays)))
	} else {
		b.WriteString("- 尚无已关闭 issue，无法评估处理效率\n")
	}

	b.WriteString("\n## 七、行动建议\n\n")
	for _, tip := range buildAdvice(data, noLabel, staleOpen) {
		b.WriteString(fmt.Sprintf("- %s\n", tip))
	}

	return b.String()
}

// BuildDeepReport 生成「选中 issue 的深度分析报告」。
func BuildDeepReport(items []issueAnalysis, meta Meta, catDist, prioDist, kwDist, ageDist []NameValue, aiInsight string) string {
	var b strings.Builder

	b.WriteString("# 选中 Issue 深度分析报告\n\n")
	b.WriteString(fmt.Sprintf("> 仓库：**%s** ｜ 选中 %d 个 issue ｜ 生成时间：%s\n",
		meta.FullName, len(items), meta.GeneratedAt))

	b.WriteString("\n## 一、整体画像\n\n")
	b.WriteString("| 维度 | 分布 |\n|---|---|\n")
	b.WriteString(fmt.Sprintf("| 分类 | %s |\n", distText(catDist)))
	b.WriteString(fmt.Sprintf("| 优先级 | %s |\n", distText(prioDist)))
	b.WriteString(fmt.Sprintf("| 处理耗时 | %s |\n", distText(ageDist)))
	if len(kwDist) > 0 {
		b.WriteString(fmt.Sprintf("| 高频关键词 | %s |\n", distText(topNToNameValue(kwDist, 10))))
	}

	if len(items) > 0 {
		urgent := make([]string, 0, len(items))
		for _, it := range items {
			if it.Priority == "P0" || it.Priority == "P1" {
				urgent = append(urgent, fmt.Sprintf("[#%d](%s)", it.Number, it.URL))
			}
		}
		b.WriteString("\n## 二、处置顺序建议\n\n")
		if len(urgent) > 0 {
			b.WriteString(fmt.Sprintf("优先处理 P0/P1：%s\n", strings.Join(urgent, "、")))
		} else {
			b.WriteString("本批 issue 中没有 P0/P1，可按常规节奏排期。\n")
		}
	}

	if strings.TrimSpace(aiInsight) != "" {
		b.WriteString("\n## 三、AI 洞察\n\n")
		b.WriteString(strings.TrimSpace(aiInsight))
		b.WriteString("\n")
	}

	b.WriteString("\n## 四、逐项分析\n\n")
	if len(items) == 0 {
		b.WriteString("未选择任何 issue。\n")
	}
	for _, it := range items {
		b.WriteString(fmt.Sprintf("### [#%d](%s) %s\n\n", it.Number, it.URL, it.Title))
		b.WriteString(fmt.Sprintf("- 分类 / 优先级：**%s** / **%s**（%d 分）\n", it.Category, it.Priority, it.Score))
		b.WriteString(fmt.Sprintf("- 状态：%s ｜ 创建：%s ｜ %s ｜ 作者：@%s\n",
			it.State, it.CreatedAt, ageLine(it), it.Author))
		b.WriteString(fmt.Sprintf("- 规模：正文约 %d 字，代码块 %d 处，评论 %d 条，参与 %d 人%s\n",
			it.Words, it.CodeBlocks, it.Comments, it.Participants, stackLine(it)))
		if len(it.Labels) > 0 {
			b.WriteString(fmt.Sprintf("- 标签：%s\n", strings.Join(mapLabels(it.Labels), "、")))
		}
		if len(it.Keywords) > 0 {
			b.WriteString(fmt.Sprintf("- 关键词：%s\n", strings.Join(it.Keywords, "、")))
		}
		b.WriteString("- 判定依据：\n")
		for _, r := range it.Reasons {
			b.WriteString(fmt.Sprintf("  - %s\n", r))
		}
		if strings.TrimSpace(it.AIInsight) != "" {
			b.WriteString(fmt.Sprintf("- AI 洞察：%s\n", strings.TrimSpace(it.AIInsight)))
		}
		b.WriteString("\n")
	}

	return b.String()
}

func ageLine(it issueAnalysis) string {
	if it.State == "closed" {
		return fmt.Sprintf("处理耗时 %d 天", it.Days)
	}
	return fmt.Sprintf("已存活 %d 天", it.Days)
}

func stackLine(it issueAnalysis) string {
	if it.HasStack {
		return "，含堆栈/日志"
	}
	return ""
}

func mapLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, fmt.Sprintf("`%s`", l))
	}
	return out
}

func topNToNameValue(in []NameValue, n int) []NameValue {
	if n > 0 && len(in) > n {
		return in[:n]
	}
	return in
}

func distText(items []NameValue) string {
	if len(items) == 0 {
		return "暂无数据"
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s × %d", it.Name, it.Value))
	}
	return strings.Join(parts, "、")
}

func buildAdvice(data DashboardData, noLabel, staleOpen int) []string {
	s := data.Summary
	var tips []string

	if s.Total == 0 {
		return []string{"仓库暂无 issue，可先建立 issue 模板与标签体系，方便后续沉淀。"}
	}
	if s.OpenRate >= 60 {
		tips = append(tips, fmt.Sprintf("未关闭占比高达 %.1f%%，建议先集中清理积压 issue，再承接新需求。", s.OpenRate))
	}
	if staleOpen > 0 {
		tips = append(tips, fmt.Sprintf("有 %d 个 issue 存活超过 180 天，建议逐个确认：是修复、关闭还是转为长期规划。", staleOpen))
	}
	if noLabel > 0 {
		tips = append(tips, fmt.Sprintf("有 %d 个 issue 没有标签（占 %.1f%%），建议补齐分类标签，便于按模块分摊与统计。", noLabel, pct(noLabel, s.Total)))
	}
	if s.CloseDays != nil && *s.CloseDays > 30 {
		tips = append(tips, fmt.Sprintf("平均关闭耗时 %.1f 天偏长，可考虑给高频模块指定 owner 并设定 SLA。", *s.CloseDays))
	}
	if len(data.LabelDist) > 0 {
		top := data.LabelDist[0]
		tips = append(tips, fmt.Sprintf("标签 `%s` 出现最多（%d 次），说明这是当前的主要矛盾点，值得单独跟进。", top.Name, top.Value))
	}
	if s.Contributors > 0 && s.Total > 0 {
		tips = append(tips, fmt.Sprintf("共有 %d 位创建者，人均 %.1f 个 issue，可据此评估社区活跃度与维护压力。",
			s.Contributors, float64(s.Total)/float64(s.Contributors)))
	}
	if len(tips) == 0 {
		tips = append(tips, "整体指标健康：保持当前的标签与关闭节奏即可。")
	}
	return tips
}

func closeSpeedComment(days float64) string {
	switch {
	case days <= 3:
		return "响应速度很快，可继续维持"
	case days <= 14:
		return "响应速度处于正常区间"
	case days <= 45:
		return "响应偏慢，建议排查人力或流程瓶颈"
	default:
		return "响应明显滞后，建议纳入专项治理"
	}
}

func recentAvg(monthly []MonthlyPoint, n int) float64 {
	if len(monthly) == 0 || n <= 0 {
		return 0
	}
	start := 0
	if len(monthly) > n {
		start = len(monthly) - n
	}
	sum := 0
	for _, m := range monthly[start:] {
		sum += m.Created
	}
	return round1(float64(sum) / float64(len(monthly)-start))
}

func medianCloseDays(issues []IssueView) float64 {
	var days []float64
	for _, it := range issues {
		if it.State == "closed" && it.Days != nil {
			days = append(days, float64(*it.Days))
		}
	}
	if len(days) == 0 {
		return 0
	}
	sort.Float64s(days)
	mid := len(days) / 2
	if len(days)%2 == 1 {
		return round1(days[mid])
	}
	return round1((days[mid-1] + days[mid]) / 2)
}

func floatPtrOrNil(f float64) *float64 {
	if f == 0 {
		return nil
	}
	return &f
}

func daysText(p *float64) string {
	if p == nil {
		return "暂无数据"
	}
	return fmt.Sprintf("%.1f 天", *p)
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return round1(float64(a) / float64(b) * 100)
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// escapeTableCell 让标题里的竖线不会破坏 Markdown 表格结构。
func escapeTableCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if utf8.RuneCountInString(s) > 60 {
		s = string([]rune(s)[:60]) + "…"
	}
	return s
}

func labelCell(labels []string) string {
	if len(labels) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, fmt.Sprintf("`%s`", l))
	}
	return strings.Join(parts, "、")
}
