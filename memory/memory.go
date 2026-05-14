// Package memory provides the Hermes/general/static memory resolution stack.
// It produces a MemoryBundle — a prioritised set of context sections — that
// agents inject into prompts for session continuity.
package memory

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/engine"
	"github.com/chimerakang/alice-core/hermes"
)

const (
	defaultMemoryBudgetChars = 6000
	staticHintBudgetChars    = 1800
	contextMaxChars          = 2000

	previousContextHeader = "[Previous conversation context]"
	previousContextLabel  = "[Previous conversation context — continuing from where we left off]\n"
	currentRequestHeader  = "[Current request]"
)

// ── Public interfaces ─────────────────────────────────────────────────────────

// Resolver resolves the memory context for a given request.
type Resolver interface {
	Resolve(ctx context.Context, req Request) (Bundle, error)
}

// HermesTaskSource provides Hermes task history for a chat.
type HermesTaskSource interface {
	GetActiveForChat(chatID int64) (hermes.TaskState, error)
	ListForChat(chatID int64, limit int) ([]hermes.TaskState, error)
}

// GeneralMemorySource provides general task memory cards.
type GeneralMemorySource interface {
	ListGeneralMemoryCards(ctx context.Context, req Request, limit int) ([]Card, error)
}

// StaticHintSource provides project-level static hints (e.g. CLAUDE.md).
type StaticHintSource interface {
	ListStaticMemoryHints(ctx context.Context, req Request, limit int) ([]StaticHint, error)
}

// ── Data types ────────────────────────────────────────────────────────────────

// Request describes the current agent context.
type Request struct {
	ChannelID      int64
	TopicID        int
	ProjectDir     string
	UserMessage    string
	IssueNumber    int
	Mode           string
	BudgetChars    int
	RecentMessages []engine.ContextMessage
}

// Bundle is a prioritised set of context sections.
type Bundle struct {
	Sections []Section
}

// Render returns the bundle text, sorted by priority.
func (b Bundle) Render() string {
	if len(b.Sections) == 0 {
		return ""
	}
	sections := make([]Section, 0, len(b.Sections))
	for _, s := range b.Sections {
		if strings.TrimSpace(s.Text) != "" {
			sections = append(sections, s)
		}
	}
	sort.SliceStable(sections, func(i, j int) bool {
		return sections[i].Priority > sections[j].Priority
	})
	var parts []string
	for _, s := range sections {
		parts = append(parts, strings.TrimSpace(s.Text))
	}
	return strings.Join(parts, "\n\n")
}

// RenderForPrompt wraps the bundle around currentRequest with headers.
func (b Bundle) RenderForPrompt(currentRequest string) string {
	currentRequest = strings.TrimSpace(currentRequest)
	if currentRequest == "" {
		return ""
	}
	rendered := strings.TrimSpace(b.Render())
	if rendered == "" {
		return currentRequest
	}
	var sb strings.Builder
	sb.WriteString(previousContextHeader)
	sb.WriteString("\n")
	sb.WriteString(rendered)
	sb.WriteString("\n\n")
	sb.WriteString(currentRequestHeader)
	sb.WriteString("\n")
	sb.WriteString(currentRequest)
	return sb.String()
}

// Section is one prioritised memory segment.
type Section struct {
	Source   string
	Scope    string
	Priority int
	Text     string
}

// Card is a general (non-Hermes) task memory entry.
type Card struct {
	ID                string
	ChannelID         int64
	TopicID           int
	ProjectDir        string
	GithubIssueNumber int
	Goal              string
	Result            string
	Engine            string
	Backend           string
	Model             string
	Files             []string
	ContinuationHints []string
	UpdatedAt         time.Time
}

// StaticHint is a project-level file hint (e.g. CLAUDE.md).
type StaticHint struct {
	Path string
	Text string
}

// ── UnifiedMemoryResolver ─────────────────────────────────────────────────────

// UnifiedMemoryResolver combines Hermes task, general, and static sources.
type UnifiedMemoryResolver struct {
	tasks   HermesTaskSource
	general GeneralMemorySource
	static  StaticHintSource
}

// New creates a resolver with only the Hermes task source.
func New(tasks HermesTaskSource) *UnifiedMemoryResolver {
	return &UnifiedMemoryResolver{tasks: tasks}
}

// NewWithGeneral adds a general memory source.
func NewWithGeneral(tasks HermesTaskSource, general GeneralMemorySource) *UnifiedMemoryResolver {
	return &UnifiedMemoryResolver{tasks: tasks, general: general}
}

// NewWithAll creates a fully-populated resolver.
func NewWithAll(tasks HermesTaskSource, general GeneralMemorySource, static StaticHintSource) *UnifiedMemoryResolver {
	return &UnifiedMemoryResolver{tasks: tasks, general: general, static: static}
}

// NewForSessionPolicy creates a resolver respecting session policy flags.
func NewForSessionPolicy(tasks HermesTaskSource, projectDir string, decision engine.SessionPolicyDecision) *UnifiedMemoryResolver {
	var general GeneralMemorySource
	if decision.AllowGeneralMemory {
		general = nil // caller injects; no global here
	}
	var static StaticHintSource
	if decision.AllowStaticHints && strings.TrimSpace(projectDir) != "" {
		static = ProjectStaticHintSource{}
	}
	return NewWithAll(tasks, general, static)
}

// Resolve builds a memory bundle for the given request.
func (r *UnifiedMemoryResolver) Resolve(ctx context.Context, req Request) (Bundle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	budget := req.BudgetChars
	if budget <= 0 {
		budget = defaultMemoryBudgetChars
	}
	issueNumber := resolvedIssueNumber(req)

	var sections []Section

	if r != nil && r.tasks != nil {
		s, err := r.hermesTaskSection(ctx, req)
		if err != nil {
			return Bundle{}, err
		}
		if strings.TrimSpace(s.Text) != "" {
			sections = append(sections, s)
		}
	}

	if r != nil && r.general != nil {
		s, err := r.generalSection(ctx, req)
		if err != nil {
			return Bundle{}, err
		}
		if strings.TrimSpace(s.Text) != "" {
			sections = append(sections, s)
		}
	}

	if r != nil && r.static != nil {
		s, err := r.staticSection(ctx, req)
		if err != nil {
			return Bundle{}, err
		}
		if strings.TrimSpace(s.Text) != "" {
			sections = append(sections, s)
		}
	}

	if issueNumber == 0 {
		bridge := strings.TrimSpace(BuildContextBridge(req.RecentMessages))
		if bridge != "" {
			sections = append(sections, Section{
				Source:   "recent_messages",
				Scope:    scopeForRequest(req),
				Priority: 20,
				Text:     clampText(bridge, contextMaxChars),
			})
		}
	} else if len(req.RecentMessages) > 0 {
		log.Printf("[memory] skipped recent_messages for explicit issue scope issue=%d channel=%d topic=%d",
			issueNumber, req.ChannelID, req.TopicID)
	}

	bundle := Bundle{Sections: clampSections(sections, budget)}
	log.Printf("[memory] resolved sections=%d mode=%s channel=%d topic=%d issue=%d budget=%d",
		len(bundle.Sections), req.Mode, req.ChannelID, req.TopicID, issueNumber, budget)
	return bundle, nil
}

func (r *UnifiedMemoryResolver) hermesTaskSection(ctx context.Context, req Request) (Section, error) {
	select {
	case <-ctx.Done():
		return Section{}, ctx.Err()
	default:
	}
	if resolvedIssueNumber(req) == 0 {
		return Section{}, nil
	}
	tasks, err := r.loadHermesTasks(req)
	if err != nil {
		return Section{}, err
	}
	if len(tasks) == 0 {
		return Section{}, nil
	}
	issueNumber := resolvedIssueNumber(req)
	scope := "chat"
	source := "hermes_task"
	priority := 80
	if issueNumber > 0 && tasks[0].GithubIssueNumber == issueNumber {
		scope = fmt.Sprintf("issue:%d", issueNumber)
		source = "issue_task"
		priority = 100
	}
	return Section{Source: source, Scope: scope, Priority: priority, Text: buildHermesTaskContextSection(tasks)}, nil
}

func (r *UnifiedMemoryResolver) generalSection(ctx context.Context, req Request) (Section, error) {
	select {
	case <-ctx.Done():
		return Section{}, ctx.Err()
	default:
	}
	cards, err := r.general.ListGeneralMemoryCards(ctx, req, 3)
	if err != nil {
		return Section{}, fmt.Errorf("list general memory: %w", err)
	}
	if len(cards) == 0 {
		return Section{}, nil
	}
	issueNumber := resolvedIssueNumber(req)
	scope := scopeForRequest(req)
	priority := 60
	if issueNumber > 0 {
		scope = fmt.Sprintf("issue:%d", issueNumber)
		priority = 90
	}
	return Section{Source: "general_task", Scope: scope, Priority: priority, Text: buildGeneralMemoryContextSection(cards)}, nil
}

func (r *UnifiedMemoryResolver) staticSection(ctx context.Context, req Request) (Section, error) {
	select {
	case <-ctx.Done():
		return Section{}, ctx.Err()
	default:
	}
	hints, err := r.static.ListStaticMemoryHints(ctx, req, 3)
	if err != nil {
		return Section{}, fmt.Errorf("list static hints: %w", err)
	}
	text := buildStaticHintContextSection(hints)
	if strings.TrimSpace(text) == "" {
		return Section{}, nil
	}
	return Section{Source: "static_hint", Scope: "project:" + req.ProjectDir, Priority: 35, Text: text}, nil
}

func (r *UnifiedMemoryResolver) loadHermesTasks(req Request) ([]hermes.TaskState, error) {
	issueNumber := resolvedIssueNumber(req)
	hasIssue := issueNumber > 0

	var tasks []hermes.TaskState
	active, err := r.tasks.GetActiveForChat(req.ChannelID)
	switch {
	case err == nil:
		if !hasIssue || active.GithubIssueNumber == issueNumber {
			tasks = append(tasks, active)
		}
	case err != hermes.ErrNoTask:
		return nil, fmt.Errorf("load active hermes memory: %w", err)
	}

	historyLimit := 3
	if hasIssue {
		historyLimit = 10
	}
	history, err := r.tasks.ListForChat(req.ChannelID, historyLimit)
	if err != nil {
		return nil, fmt.Errorf("list hermes memory: %w", err)
	}

	currentNorm := normalizeGoal(req.UserMessage)
	seen := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		seen[t.ID] = struct{}{}
	}
	if hasIssue {
		for _, t := range history {
			if t.GithubIssueNumber != issueNumber {
				continue
			}
			if _, ok := seen[t.ID]; ok {
				continue
			}
			tasks = append(tasks, t)
			seen[t.ID] = struct{}{}
			if len(tasks) >= 3 {
				return tasks, nil
			}
		}
		return tasks, nil
	}
	for _, t := range history {
		if _, ok := seen[t.ID]; ok {
			continue
		}
		if normalizeGoal(extractActionableGoal(t.Goal)) == currentNorm {
			continue
		}
		tasks = append(tasks, t)
		if len(tasks) >= 2 {
			break
		}
	}
	return tasks, nil
}

// ── ProjectStaticHintSource ───────────────────────────────────────────────────

// ProjectStaticHintSource reads CLAUDE.md and docs/arch/memory.md.
type ProjectStaticHintSource struct{}

// ListStaticMemoryHints implements StaticHintSource.
func (ProjectStaticHintSource) ListStaticMemoryHints(ctx context.Context, req Request, limit int) ([]StaticHint, error) {
	projectDir := strings.TrimSpace(req.ProjectDir)
	if projectDir == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 3
	}
	paths := []string{"CLAUDE.md", filepath.Join("docs", "arch", "memory.md")}
	var hints []StaticHint
	for _, rel := range paths {
		if len(hints) >= limit {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		full := filepath.Join(projectDir, rel)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() || info.Size() > 256*1024 {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		if text := strings.TrimSpace(string(data)); text != "" {
			hints = append(hints, StaticHint{Path: filepath.ToSlash(rel), Text: text})
		}
	}
	return hints, nil
}

// ── Exported helpers ──────────────────────────────────────────────────────────

// BuildContextBridge produces a context summary from recent messages.
// Used when injecting prior conversation into a new session.
func BuildContextBridge(messages []engine.ContextMessage) string {
	if len(messages) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(previousContextLabel)
	for _, msg := range messages {
		role := "User"
		if msg.Role == "assistant" {
			role = "Assistant"
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", role, msg.Content))
	}
	sb.WriteString("\n---\n\n")
	return sb.String()
}

// ParseIssueNumber extracts a GitHub issue number from text (e.g. "#42").
func ParseIssueNumber(text string) (int, bool) {
	match := issueRefPattern.FindString(text)
	if match == "" {
		return 0, false
	}
	digits := []rune(match)[1:]
	n := 0
	for _, r := range digits {
		switch {
		case r >= '0' && r <= '9':
			n = n*10 + int(r-'0')
		case r >= '０' && r <= '９':
			n = n*10 + int(r-'０')
		default:
			return 0, false
		}
	}
	if n == 0 {
		return 0, false
	}
	return n, true
}

var issueRefPattern = regexp.MustCompile(`[#＃][0-9０-９]+`)

// ── Private helpers ───────────────────────────────────────────────────────────

func resolvedIssueNumber(req Request) int {
	if req.IssueNumber > 0 {
		return req.IssueNumber
	}
	n, _ := ParseIssueNumber(req.UserMessage)
	return n
}

func scopeForRequest(req Request) string {
	if n := resolvedIssueNumber(req); n > 0 {
		return fmt.Sprintf("issue:%d", n)
	}
	if req.TopicID != 0 {
		return fmt.Sprintf("channel:%d/topic:%d", req.ChannelID, req.TopicID)
	}
	return fmt.Sprintf("channel:%d", req.ChannelID)
}

func clampSections(sections []Section, budget int) []Section {
	if budget <= 0 || len(sections) == 0 {
		return sections
	}
	sort.SliceStable(sections, func(i, j int) bool {
		return sections[i].Priority > sections[j].Priority
	})
	var out []Section
	remaining := budget
	for _, s := range sections {
		text := strings.TrimSpace(s.Text)
		if text == "" || remaining <= 0 {
			continue
		}
		if len([]rune(text)) > remaining {
			text = clampText(text, remaining)
		}
		s.Text = text
		out = append(out, s)
		remaining -= len([]rune(text))
	}
	return out
}

func clampText(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return strings.TrimSpace(string(runes[:maxRunes-3])) + "..."
}

func normalizeGoal(goal string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(goal)), " ")
}

func extractActionableGoal(goal string) string {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return ""
	}
	if strings.HasPrefix(goal, "[Hermes continuation]") {
		const origHeader = "Original goal:"
		const progHeader = "\n\nCurrent progress:"
		if start := strings.Index(goal, origHeader); start >= 0 {
			actionable := strings.TrimSpace(goal[start+len(origHeader):])
			if end := strings.Index(actionable, progHeader); end >= 0 {
				actionable = strings.TrimSpace(actionable[:end])
			}
			if actionable != "" {
				return extractActionableGoal(actionable)
			}
		}
	}
	idx := strings.LastIndex(goal, currentRequestHeader)
	if idx < 0 {
		return goal
	}
	actionable := strings.TrimSpace(goal[idx+len(currentRequestHeader):])
	if actionable == "" {
		return goal
	}
	return actionable
}

func indentText(s, prefix string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

var recursiveMarkers = []string{
	"# Hermes Executor Rules",
	"Hermes Executor Rules (Codex)",
	"[Hermes continuation]",
	"=== Hermes Executor 上下文 ===",
}

func cardHasRecursiveContent(card Card) bool {
	for _, marker := range recursiveMarkers {
		if strings.Contains(card.Goal, marker) || strings.Contains(card.Result, marker) {
			return true
		}
	}
	return false
}

func buildGeneralMemoryContextSection(cards []Card) string {
	if len(cards) == 0 {
		return ""
	}
	var sections []string
	for _, card := range cards {
		if cardHasRecursiveContent(card) {
			continue
		}
		goal := strings.TrimSpace(stripLanguageDirective(card.Goal))
		result := strings.TrimSpace(card.Result)
		if goal == "" && result == "" {
			continue
		}
		var sb strings.Builder
		sb.WriteString("Persisted general work memory")
		if !card.UpdatedAt.IsZero() {
			sb.WriteString(" (")
			sb.WriteString(card.UpdatedAt.Format(time.RFC3339))
			sb.WriteString(")")
		}
		sb.WriteString(":\n")
		if card.ID != "" {
			sb.WriteString("- Task ID: " + card.ID + "\n")
		}
		if card.Engine != "" {
			sb.WriteString("- Engine: " + card.Engine + "\n")
		}
		if card.Model != "" {
			sb.WriteString("- Model: " + card.Model + "\n")
		}
		if card.GithubIssueNumber > 0 {
			sb.WriteString(fmt.Sprintf("- GitHub issue: #%d\n", card.GithubIssueNumber))
		}
		if len(card.Files) > 0 {
			sb.WriteString("- Touched files:\n")
			for _, p := range card.Files {
				sb.WriteString("  - " + clampText(p, 240) + "\n")
			}
		}
		if len(card.ContinuationHints) > 0 {
			sb.WriteString("- Continuation hints:\n")
			for _, h := range card.ContinuationHints {
				sb.WriteString("  - " + clampText(h, 320) + "\n")
			}
		}
		if goal != "" {
			sb.WriteString("- Request: " + clampText(goal, 800) + "\n")
		}
		if result != "" {
			sb.WriteString("- Result:\n" + indentText(clampText(result, 1200), "  "))
		}
		sections = append(sections, strings.TrimSpace(sb.String()))
	}
	if len(sections) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Persisted general task context:\n")
	sb.WriteString("Use this as continuity for related direct/file/media work. Treat it as a summary, and verify concrete details before editing or making high-impact decisions.\n\n")
	sb.WriteString(strings.Join(sections, "\n\n"))
	return sb.String()
}

func buildStaticHintContextSection(hints []StaticHint) string {
	if len(hints) == 0 {
		return ""
	}
	var sections []string
	for _, hint := range hints {
		path := strings.TrimSpace(hint.Path)
		text := strings.TrimSpace(hint.Text)
		if path == "" || text == "" {
			continue
		}
		var sb strings.Builder
		sb.WriteString("Static project hint from " + path + ":\n")
		sb.WriteString(clampText(text, staticHintBudgetChars))
		sections = append(sections, strings.TrimSpace(sb.String()))
	}
	if len(sections) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Static project context:\n")
	sb.WriteString("Use these attributed project hints as low-priority orientation. Prefer live code and task memory for concrete implementation details.\n\n")
	sb.WriteString(strings.Join(sections, "\n\n"))
	return sb.String()
}

func buildHermesTaskContextSection(tasks []hermes.TaskState) string {
	if len(tasks) == 0 {
		return ""
	}
	var sections []string
	for _, task := range tasks {
		goal := strings.TrimSpace(extractActionableGoal(task.Goal))
		progress := strings.TrimSpace(buildProgressSummary(task))
		if goal == "" && progress == "" {
			continue
		}
		var sb strings.Builder
		sb.WriteString("Persisted Hermes work memory")
		if !task.UpdatedAt.IsZero() {
			sb.WriteString(" (" + task.UpdatedAt.Format(time.RFC3339) + ")")
		}
		sb.WriteString(":\n")
		if task.ID != "" {
			sb.WriteString("- Task ID: " + task.ID + "\n")
		}
		if task.Status != "" {
			sb.WriteString("- Status: " + string(task.Status) + "\n")
		}
		if task.GithubIssueNumber > 0 {
			sb.WriteString(fmt.Sprintf("- GitHub issue: #%d\n", task.GithubIssueNumber))
		}
		if goal != "" {
			sb.WriteString("- Original request: " + clampText(goal, contextMaxChars) + "\n")
		}
		if progress != "" {
			sb.WriteString("- Stored progress:\n" + indentText(clampText(progress, contextMaxChars), "  "))
		}
		sections = append(sections, strings.TrimSpace(sb.String()))
	}
	if len(sections) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Persisted Hermes context:\n")
	sb.WriteString("Use this as continuity for the same issue/topic. Start from this stored state, avoid broad rediscovery, and only re-read files or GitHub issue details when a specific uncertainty must be verified.\n\n")
	sb.WriteString(strings.Join(sections, "\n\n"))
	return sb.String()
}

func buildProgressSummary(task hermes.TaskState) string {
	var lines []string
	if acc := strings.TrimSpace(task.Accumulated); acc != "" {
		lines = append(lines, "Accumulated summary:\n"+clampText(acc, contextMaxChars))
	}
	if len(task.Plan) > 0 {
		lines = append(lines, "Subtasks:")
		for i, sub := range task.Plan {
			desc := strings.TrimSpace(sub.Description)
			if desc == "" {
				desc = "(no description)"
			}
			line := fmt.Sprintf("%d. [%s] %s", i+1, sub.Status, desc)
			if result := strings.TrimSpace(sub.Result); result != "" {
				line += "\n   Result: " + clampText(result, 600)
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "(No stored progress details.)"
	}
	return strings.Join(lines, "\n")
}

var languageDirectivePrefixes = []string{
	"請用繁體中文回應。\n\n",
	"Please respond in English. Do NOT use Chinese characters or Chinese formatting in your response.\n\n",
}

func stripLanguageDirective(prompt string) string {
	for _, prefix := range languageDirectivePrefixes {
		if strings.HasPrefix(prompt, prefix) {
			return prompt[len(prefix):]
		}
	}
	return prompt
}
