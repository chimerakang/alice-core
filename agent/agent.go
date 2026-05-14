package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chimerakang/alice-core/client"
	"github.com/chimerakang/alice-core/engine"
	"github.com/chimerakang/alice-core/memory"
)

// directPlanViaGraph reads the ALICE_DIRECT_PLAN_VIA_GRAPH env flag.
func directPlanViaGraph() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("ALICE_DIRECT_PLAN_VIA_GRAPH")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// projectState holds per-project session state and token stats.
type projectState struct {
	ctx              *engine.ChatContext
	stats            TokenStats
	lastTotalCostUSD float64
	lastCostSession  string
}

func (ps *projectState) recordCallCost(resp *client.CLIResponse, model string) float64 {
	if ps == nil || resp == nil {
		return 0
	}
	var deltaCost float64
	sameSession := resp.SessionID != "" && resp.SessionID == ps.lastCostSession
	if sameSession {
		deltaCost = resp.TotalCostUSD - ps.lastTotalCostUSD
	} else {
		deltaCost = resp.TotalCostUSD
	}
	if deltaCost <= 0 {
		deltaCost = EstimateClaudeCost(
			model,
			resp.Usage.InputTokens,
			resp.Usage.CacheReadInputTokens,
			resp.Usage.CacheCreationInputTokens,
			resp.Usage.OutputTokens,
		)
		if deltaCost < 0 {
			log.Printf("[agent] cost delta negative (total=%.4f last=%.4f same_session=%v); using estimate=%.4f",
				resp.TotalCostUSD, ps.lastTotalCostUSD, sameSession, deltaCost)
		}
	}
	ps.lastTotalCostUSD = resp.TotalCostUSD
	ps.lastCostSession = resp.SessionID
	return deltaCost
}

// Agent coordinates AI CLI calls for a single channel/project context.
type Agent struct {
	client               client.Client
	projectDir           string
	projects             map[string]*projectState
	channelID            int64
	topicID              int
	currentModelOverride string
	lastUsedModel        string
	chatContext          *engine.ChatContext
	enablePlanMode       bool
	planModel            string
	executeModel         string
	cliTimeoutMinutes    int
	stickySession        bool
	sessionIdleTimeout   time.Duration
	runMu                sync.Mutex
	cancelFunc           context.CancelFunc
	cancelMu             sync.Mutex
	execution            *engine.ExecutionLifecycle

	// Optional injected dependencies — all nil-safe.
	storage  StorageBackend
	events   EventSink
	metrics  MetricsRecorder
	pii      PIIFilter
	skills   SkillMatcher
	decLog   *DecisionLogger
	toolLog  *ToolLogger

	// Per-call metrics.
	lastCallMu         sync.Mutex
	lastCallModel      string
	lastCallInputT     int
	lastCallOutputT    int
	lastCallCost       float64
	lastCallCacheRead  int
	lastCallCacheWrite int

	suppressMemoryBridge bool
}

// Options configures optional dependencies for an Agent.
type Options struct {
	Storage  StorageBackend
	Events   EventSink
	Metrics  MetricsRecorder
	PIIFilter PIIFilter
	Skills   SkillMatcher
	// DecisionLogSize controls in-memory decision log capacity (default 50).
	DecisionLogSize int
	// ToolLogSize controls in-memory tool log capacity (default 100).
	ToolLogSize int
}

// NewAgent creates an Agent with the given client, project directory, and channel/topic IDs.
func NewAgent(c client.Client, projectDir string, channelID int64, topicID int) *Agent {
	return NewAgentWithContext(c, engine.NewChatContext(channelID, topicID, projectDir), Options{})
}

// NewAgentWithOptions creates an Agent with full option control.
func NewAgentWithOptions(c client.Client, projectDir string, channelID int64, topicID int, opts Options) *Agent {
	return NewAgentWithContext(c, engine.NewChatContext(channelID, topicID, projectDir), opts)
}

// NewAgentWithContext creates an Agent from an existing ChatContext.
func NewAgentWithContext(c client.Client, chatCtx *engine.ChatContext, opts Options) *Agent {
	if chatCtx == nil {
		chatCtx = engine.NewChatContext(0, 0, "")
	}
	dlSize := opts.DecisionLogSize
	if dlSize <= 0 {
		dlSize = 50
	}
	tlSize := opts.ToolLogSize
	if tlSize <= 0 {
		tlSize = 100
	}
	return &Agent{
		client:             c,
		projectDir:         chatCtx.ProjectDir,
		projects:           make(map[string]*projectState),
		channelID:          chatCtx.ChatID,
		topicID:            chatCtx.ThreadID,
		chatContext:        chatCtx,
		stickySession:      true,
		sessionIdleTimeout: 24 * time.Hour,
		execution:          engine.NewExecutionLifecycle(),
		storage:            opts.Storage,
		events:             opts.Events,
		metrics:            opts.Metrics,
		pii:                opts.PIIFilter,
		skills:             opts.Skills,
		decLog:             NewDecisionLogger(dlSize),
		toolLog:            NewToolLogger(tlSize),
	}
}

// SetSuppressMemoryBridge controls context bridge injection on model/session switches.
func (a *Agent) SetSuppressMemoryBridge(v bool) { a.suppressMemoryBridge = v }

// SetModelOverride sets an explicit model for the next run.
func (a *Agent) SetModelOverride(model string) { a.currentModelOverride = model }

// SetRoutingConfig applies sticky-session routing behaviour.
func (a *Agent) SetRoutingConfig(cfg ModelRoutingConfig) {
	a.stickySession = cfg.StickyEnabled()
	a.sessionIdleTimeout = time.Duration(cfg.IdleTimeoutMinutes()) * time.Minute
}

// SetPlanMode enables two-phase plan/execute with the given model names.
func (a *Agent) SetPlanMode(enabled bool, planModel, executeModel string) {
	a.enablePlanMode = enabled
	a.planModel = planModel
	a.executeModel = executeModel
}

// IsPlanMode reports whether two-phase planning is enabled.
func (a *Agent) IsPlanMode() bool { return a.enablePlanMode }

// SetProject switches the working directory.
func (a *Agent) SetProject(dir string) {
	a.projectDir = dir
	if a.chatContext != nil {
		a.chatContext.ProjectDir = dir
	}
}

// Reset clears session and stat state for the current project.
func (a *Agent) Reset() {
	delete(a.projects, a.projectDir)
	a.currentModelOverride = ""
	a.lastUsedModel = ""
	a.enablePlanMode = false
	a.planModel = ""
	a.executeModel = ""
	if a.chatContext != nil {
		a.chatContext.Sessions = make(map[engine.BackendKind]string)
		a.chatContext.RecentMsgs = nil
		a.chatContext.LastActivity = time.Now()
	}
}

// ClearSession resets backend session IDs while preserving usage stats.
func (a *Agent) ClearSession() {
	ps := a.current()
	ps.ctx.Sessions = make(map[engine.BackendKind]string)
	ps.ctx.RecentMsgs = nil
	a.currentModelOverride = ""
	a.lastUsedModel = ""
	a.enablePlanMode = false
	a.planModel = ""
	a.executeModel = ""
}

// Abort cancels any in-flight CLI subprocess.
func (a *Agent) Abort() bool {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.cancelFunc != nil {
		a.transitionExecution(engine.ExecutionStateCancelling, "agent_abort")
		a.cancelFunc()
		a.cancelFunc = nil
		return true
	}
	return false
}

// IsProcessing reports whether the agent is running a task.
func (a *Agent) IsProcessing() bool {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	return a.executionLifecycle().IsProcessing()
}

// LastUsedModel returns the model from the most recent run.
func (a *Agent) LastUsedModel() string { return a.lastUsedModel }

// Stats returns cumulative token stats for the current project.
func (a *Agent) Stats() TokenStats { return a.current().stats }

// SessionID returns the active session ID for the current backend.
func (a *Agent) SessionID() string {
	ps := a.current()
	return ps.ctx.Session(ps.ctx.LastBackend)
}

// ProjectDir returns the agent's working directory.
func (a *Agent) ProjectDir() string { return a.projectDir }

// LastActivity returns the timestamp of the most recent activity.
func (a *Agent) LastActivity() time.Time { return a.current().ctx.LastActivity }

// AddRecentMessage appends one exchange to the conversation bridge.
func (a *Agent) AddRecentMessage(userMsg, assistantMsg string) {
	addToRecentMessages(a.current(), userMsg, assistantMsg)
}

// RecentMessages returns a snapshot of the conversation bridge.
func (a *Agent) RecentMessages() []engine.ContextMessage {
	return a.current().ctx.RecentMessagesSnapshot()
}

// LastCallMetrics returns token/cost info from the most recent CLI call.
func (a *Agent) LastCallMetrics() (model string, inTokens, outTokens int, cost float64) {
	a.lastCallMu.Lock()
	defer a.lastCallMu.Unlock()
	return a.lastCallModel, a.lastCallInputT, a.lastCallOutputT, a.lastCallCost
}

// LastCacheMetrics returns cache_read + cache_write tokens from the last call.
func (a *Agent) LastCacheMetrics() (cacheRead, cacheWrite int) {
	a.lastCallMu.Lock()
	defer a.lastCallMu.Unlock()
	return a.lastCallCacheRead, a.lastCallCacheWrite
}

// ToolLog exposes the in-memory tool execution log.
func (a *Agent) ToolLog() *ToolLogger { return a.toolLog }

// DecisionLog exposes the in-memory decision log.
func (a *Agent) DecisionLog() *DecisionLogger { return a.decLog }

// ---------------------------------------------------------------------------
// Run — single-phase streaming execution
// ---------------------------------------------------------------------------

// Run sends userMessage to the CLI and streams the response back.
// onUpdate(msg, silent): silent=false for progress banners, true for tool activity.
func (a *Agent) Run(userMessage string, onUpdate func(string, bool)) (string, error) {
	a.runMu.Lock()
	defer a.runMu.Unlock()

	originalUserMessage := userMessage
	startTime := time.Now()

	var ctx context.Context
	var cancel context.CancelFunc
	if a.cliTimeoutMinutes > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(a.cliTimeoutMinutes)*time.Minute)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	a.cancelMu.Lock()
	a.cancelFunc = cancel
	a.transitionExecution(engine.ExecutionStateStarting, "agent_run")
	a.cancelMu.Unlock()
	defer func() {
		a.cancelMu.Lock()
		a.cancelFunc = nil
		a.cancelMu.Unlock()
		cancel()
	}()

	ps := a.current()
	_ = ps.ctx.TransitionState(engine.ChatStateBusy, "agent_run")
	finalChatState := engine.ChatStateIdle
	defer func() {
		_ = ps.ctx.TransitionState(finalChatState, "agent_run_done")
	}()
	a.expireIdleStickySession(ps, startTime)

	runDecision := a.decideDirectRun(ps, userMessage)
	selectedModel := runDecision.Model
	routingReason := runDecision.RoutingReason
	routingLatency := runDecision.LatencyMS
	log.Printf("[agent] model routing: model=%s reason=%s latency=%dms", selectedModel, routingReason, routingLatency)

	if onUpdate != nil {
		onUpdate(processingStatusForModel(selectedModel), false)
	}
	selectedBackend := runDecision.Backend
	previousBackend := runDecision.PreviousBackend

	if selectedModel != a.lastUsedModel && a.lastUsedModel != "" {
		if !a.suppressMemoryBridge {
			bridge := a.resolveMemoryBridge(ctx, ps, userMessage, "direct_model_switch")
			if bridge != "" {
				userMessage = bridge + userMessage
				log.Printf("[agent] context bridge injected (%d recent messages)", len(ps.ctx.RecentMsgs))
			}
		} else {
			log.Printf("[agent] context bridge skipped (executor mode)")
		}
		if selectedBackend == previousBackend {
			ps.ctx.ClearSession(selectedBackend)
		}
	}
	if !a.suppressMemoryBridge && shouldInjectContinuationBridge(userMessage) {
		bridge := a.resolveMemoryBridge(ctx, ps, userMessage, "direct_continuation")
		if bridge != "" && !strings.HasPrefix(userMessage, bridge) {
			userMessage = bridge + userMessage
			log.Printf("[agent] continuation bridge injected (%d recent messages)", len(ps.ctx.RecentMsgs))
		}
	}
	a.lastUsedModel = selectedModel
	ps.ctx.LastBackend = selectedBackend

	if a.skills != nil {
		matched := a.skills.FindRelevantSkills(userMessage, a.projectDir)
		if len(matched) > 0 {
			userMessage = userMessage + "\n" + formatSkillsForPrompt(matched)
			log.Printf("[agent] injected %d auto-skills", len(matched))
		}
	}

	sessionID := runDecision.SessionID
	log.Printf("[agent] calling CLI session=%s project=%s model=%s", sessionID, a.projectDir, a.lastUsedModel)

	var toolCallsForDecision []ToolExecution

	const maxRetries = 2
	var resp *client.CLIResponse
	var err error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			a.transitionExecution(engine.ExecutionStateRetrying, "agent_retry")
			recovery := engine.DecideRecovery(engine.RecoveryRequest{
				Mode:        "direct_stream",
				Attempt:     attempt - 1,
				MaxAttempts: maxRetries,
				ErrorText:   err.Error(),
			})
			log.Printf("[agent] retry %d/%d after %v: %v", attempt, maxRetries, recovery.RetryAfter, err)
			if onUpdate != nil {
				onUpdate(fmt.Sprintf("retry %d/%d...", attempt, maxRetries), false)
			}
			select {
			case <-time.After(recovery.RetryAfter):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			toolCallsForDecision = nil
		}

		a.transitionExecution(engine.ExecutionStateStreaming, "agent_stream")
		resp, userMessage, sessionID, err = a.callStreamWithResumeBridge(ctx, ps, userMessage, selectedBackend, sessionID, a.lastUsedModel, func(toolName string, toolInput map[string]interface{}) {
			a.toolLog.LogStart(toolName, toolInput, a.channelID, a.topicID, a.projectDir)
			if a.events != nil {
				exec := ToolExecution{Timestamp: time.Now(), ToolName: toolName, Input: toolInput, Status: "running", ChannelID: a.channelID, TopicID: a.topicID, ProjectPath: a.projectDir}
				a.events.OnToolEvent("tool_execution_start", exec)
			}
			toolCallsForDecision = append(toolCallsForDecision, ToolExecution{
				Timestamp:   time.Now(),
				ToolName:    toolName,
				Input:       toolInput,
				Status:      "executed",
				ChannelID:   a.channelID,
				TopicID:     a.topicID,
				ProjectPath: a.projectDir,
			})
			if onUpdate != nil {
				if msg := formatToolUpdate(toolName, toolInput); msg != "" {
					onUpdate(msg, true)
				}
			}
		}, func(_, _ string) {})

		if err == nil {
			break
		}

		recoveryReq := engine.RecoveryRequest{Mode: "direct_stream", Attempt: attempt, MaxAttempts: maxRetries, ErrorText: err.Error()}
		recovery := engine.DecideRecovery(recoveryReq)
		engine.LogRecoveryDecision(recoveryReq, recovery)
		if recovery.Action == engine.RecoveryActionRetry {
			log.Printf("[agent] retryable error: %v", err)
			if resp != nil && resp.SessionID != "" {
				ps.ctx.SetSession(selectedBackend, resp.SessionID)
				sessionID = resp.SessionID
			}
			continue
		}
		break
	}

	if err != nil {
		partialText := ""
		deltaCost := 0.0
		if resp != nil {
			partialText = resp.TextContent
			if resp.SessionID != "" {
				ps.ctx.SetSession(selectedBackend, resp.SessionID)
			}
			deltaCost = ps.recordCallCost(resp, a.lastUsedModel)
			a.accumulateStats(ps, resp, deltaCost)
		}
		a.logDecision(userMessage, partialText, toolCallsForDecision, startTime, resp, err, routingReason, routingLatency, deltaCost)
		a.finishExecutionError(err, "agent_run")
		return partialText, fmt.Errorf("CLI call failed: %w", err)
	}

	ps.ctx.SetSession(selectedBackend, resp.SessionID)
	deltaCost := ps.recordCallCost(resp, a.lastUsedModel)
	a.accumulateStats(ps, resp, deltaCost)

	log.Printf("[agent] done: turns=%d in=%d cache_read=%d cache_write=%d out=%d cost=$%.4f session=%s",
		resp.NumTurns, resp.Usage.InputTokens, resp.Usage.CacheReadInputTokens,
		resp.Usage.CacheCreationInputTokens, resp.Usage.OutputTokens, deltaCost, resp.SessionID)

	a.logDecision(userMessage, resp.Result, toolCallsForDecision, startTime, resp, nil, routingReason, routingLatency, deltaCost)
	addToRecentMessages(ps, originalUserMessage, resp.Result)
	if engine.AssistantAwaitsInput(resp.Result) {
		finalChatState = engine.ChatStateAwaitingInput
	}

	a.recordLastCallMetrics(a.lastUsedModel, resp.Usage.InputTokens, resp.Usage.OutputTokens, deltaCost)
	a.recordLastCallCacheMetrics(resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)
	a.transitionExecution(engine.ExecutionStateSucceeded, "agent_run_succeeded")

	return resp.Result, nil
}

// runDirect is the single-phase fallback used by two-phase plan/execute.
func (a *Agent) runDirect(ctx context.Context, userMessage string, onUpdate func(string, bool), startTime time.Time) (string, error) {
	ps := a.current()
	selectedModel := a.executeModel
	if selectedModel == "" {
		selectedModel = a.client.GetModel()
	}
	if onUpdate != nil {
		onUpdate(processingStatusForModel(selectedModel), false)
	}
	a.lastUsedModel = selectedModel
	selectedBackend := engine.BackendKindForModel(selectedModel)
	ps.ctx.LastBackend = selectedBackend
	sessionID := ps.ctx.Session(selectedBackend)
	a.transitionExecution(engine.ExecutionStateStreaming, "agent_run_direct_stream")

	var toolCallsForDecision []ToolExecution
	resp, userMessage, sessionID, err := a.callStreamWithResumeBridge(ctx, ps, userMessage, selectedBackend, sessionID, selectedModel, func(toolName string, toolInput map[string]interface{}) {
		a.toolLog.LogStart(toolName, toolInput, a.channelID, a.topicID, a.projectDir)
		toolCallsForDecision = append(toolCallsForDecision, ToolExecution{
			Timestamp: time.Now(), ToolName: toolName, Input: toolInput, Status: "executed",
			ChannelID: a.channelID, TopicID: a.topicID, ProjectPath: a.projectDir,
		})
		if onUpdate != nil {
			if msg := formatToolUpdate(toolName, toolInput); msg != "" {
				onUpdate(msg, true)
			}
		}
	}, func(_, _ string) {})
	_ = sessionID

	if err != nil {
		partialText := ""
		deltaCost := 0.0
		if resp != nil {
			partialText = resp.TextContent
			if resp.SessionID != "" {
				ps.ctx.SetSession(selectedBackend, resp.SessionID)
			}
			deltaCost = ps.recordCallCost(resp, selectedModel)
			a.accumulateStats(ps, resp, deltaCost)
		}
		a.logDecision(userMessage, partialText, toolCallsForDecision, startTime, resp, err, "plan_fallback", 0, deltaCost)
		a.finishExecutionError(err, "agent_run_direct")
		return partialText, fmt.Errorf("CLI call failed: %w", err)
	}

	ps.ctx.SetSession(selectedBackend, resp.SessionID)
	deltaCost := ps.recordCallCost(resp, selectedModel)
	a.accumulateStats(ps, resp, deltaCost)
	a.logDecision(userMessage, resp.Result, toolCallsForDecision, startTime, resp, nil, "plan_fallback", 0, deltaCost)
	addToRecentMessages(ps, userMessage, resp.Result)
	a.recordLastCallMetrics(selectedModel, resp.Usage.InputTokens, resp.Usage.OutputTokens, deltaCost)
	a.recordLastCallCacheMetrics(resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)
	a.transitionExecution(engine.ExecutionStateSucceeded, "agent_run_direct_succeeded")
	return resp.Result, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (a *Agent) executionLifecycle() *engine.ExecutionLifecycle {
	if a.execution == nil {
		a.execution = engine.NewExecutionLifecycle()
	}
	return a.execution
}

func (a *Agent) transitionExecution(to engine.ExecutionState, reason string) {
	if err := a.executionLifecycle().Transition(to, reason); err == nil {
		return
	}
	switch to {
	case engine.ExecutionStateStarting:
		if a.executionLifecycle().Snapshot().Terminal {
			_ = a.executionLifecycle().Transition(engine.ExecutionStateStarting, reason)
		}
	case engine.ExecutionStateStreaming:
		_ = a.executionLifecycle().Transition(engine.ExecutionStateStarting, reason+"_start")
		_ = a.executionLifecycle().Transition(engine.ExecutionStateStreaming, reason)
	}
}

func (a *Agent) finishExecutionError(err error, reason string) {
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "agent aborted by user") {
		a.transitionExecution(engine.ExecutionStateCancelled, reason+"_cancelled")
		return
	}
	a.transitionExecution(engine.ExecutionStateFailed, reason+"_failed")
}

func (a *Agent) current() *projectState {
	if ps, ok := a.projects[a.projectDir]; ok {
		return ps
	}
	if a.chatContext == nil {
		a.chatContext = engine.NewChatContext(a.channelID, a.topicID, a.projectDir)
	}
	a.chatContext.ProjectDir = a.projectDir
	ps := &projectState{ctx: a.chatContext}
	a.projects[a.projectDir] = ps
	return ps
}

func (a *Agent) accumulateStats(ps *projectState, resp *client.CLIResponse, deltaCost float64) {
	ps.stats.APICallCount++
	ps.stats.TotalInputTokens += int64(resp.Usage.InputTokens)
	ps.stats.TotalCacheReadTokens += int64(resp.Usage.CacheReadInputTokens)
	ps.stats.TotalCacheCreationTokens += int64(resp.Usage.CacheCreationInputTokens)
	ps.stats.TotalOutputTokens += int64(resp.Usage.OutputTokens)
	ps.stats.TotalCostUSD += deltaCost
	ps.ctx.LastActivity = time.Now()
}

func (a *Agent) recordLastCallMetrics(model string, inT, outT int, cost float64) {
	a.lastCallMu.Lock()
	defer a.lastCallMu.Unlock()
	a.lastCallModel = model
	a.lastCallInputT = inT
	a.lastCallOutputT = outT
	a.lastCallCost = cost
	a.lastCallCacheRead = 0
	a.lastCallCacheWrite = 0
}

func (a *Agent) recordLastCallCacheMetrics(read, write int) {
	a.lastCallMu.Lock()
	defer a.lastCallMu.Unlock()
	a.lastCallCacheRead = read
	a.lastCallCacheWrite = write
}

// ---------------------------------------------------------------------------
// Session / sticky model helpers
// ---------------------------------------------------------------------------

func (a *Agent) activeStickyModel(ps *projectState) string {
	if !a.stickySession || ps == nil || ps.ctx == nil || a.lastUsedModel == "" {
		return ""
	}
	if ps.ctx.Session(engine.BackendKindForModel(a.lastUsedModel)) == "" {
		return ""
	}
	return a.lastUsedModel
}

func (a *Agent) activeContinuationModel(ps *projectState, msg string) string {
	if ps == nil || ps.ctx == nil || a.lastUsedModel == "" || !isContinuationMessage(msg) {
		return ""
	}
	if ps.ctx.Session(engine.BackendKindForModel(a.lastUsedModel)) == "" {
		return ""
	}
	return a.lastUsedModel
}

func (a *Agent) expireIdleStickySession(ps *projectState, now time.Time) {
	if !a.stickySession || ps == nil || ps.ctx == nil || a.lastUsedModel == "" {
		return
	}
	timeout := a.sessionIdleTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if ps.ctx.Session(engine.BackendKindForModel(a.lastUsedModel)) == "" {
		return
	}
	if ps.ctx.LastActivity.IsZero() || now.Sub(ps.ctx.LastActivity) <= timeout {
		return
	}
	log.Printf("[agent] sticky session expired after %s idle", now.Sub(ps.ctx.LastActivity).Round(time.Second))
	ps.ctx.ClearSession(engine.BackendKindForModel(a.lastUsedModel))
	a.lastUsedModel = ""
}

func (a *Agent) decideDirectRun(ps *projectState, userMessage string) engine.SessionRunDecision {
	req := engine.SessionRunRequest{
		OverrideModel:     a.currentModelOverride,
		StickyModel:       a.activeStickyModel(ps),
		ContinuationModel: a.activeContinuationModel(ps, userMessage),
		DefaultModel:      a.client.GetModel(),
	}
	if ps != nil && ps.ctx != nil {
		req.PreviousBackend = ps.ctx.LastBackend
		req.BackendSessions = ps.ctx.Sessions
	}
	if req.OverrideModel == "" && req.StickyModel == "" && req.ContinuationModel == "" {
		startRouting := time.Now()
		req.RoutedModel, req.RoutedReason = a.selectModel(userMessage)
		req.RoutedLatencyMS = int(time.Since(startRouting).Milliseconds())
	}
	return engine.DecideSessionRun(req)
}

func (a *Agent) selectModel(userMessage string) (string, string) {
	routes := DefaultModelRoutes()
	var bestMatch *ModelRoute
	for i := range routes {
		route := &routes[i]
		if re, err := regexp.Compile(route.Pattern); err == nil {
			if re.MatchString(userMessage) && (bestMatch == nil || route.Priority < bestMatch.Priority) {
				bestMatch = route
			}
		}
	}
	if bestMatch != nil {
		return bestMatch.Model, "static_rule"
	}
	return "sonnet", "default"
}

// ---------------------------------------------------------------------------
// Memory bridge
// ---------------------------------------------------------------------------

func (a *Agent) resolveMemoryBridge(ctx context.Context, ps *projectState, userMessage, mode string) string {
	if ps == nil || ps.ctx == nil {
		return ""
	}
	_, hasIssueScope := memory.ParseIssueNumber(userMessage)
	decision := engine.DecideSessionPolicy(engine.SessionPolicyRequest{
		Mode:          mode,
		HasIssueScope: hasIssueScope,
	})
	resolver := memory.NewForSessionPolicy(nil, ps.ctx.ProjectDir, decision)
	recentMessages := ps.ctx.RecentMessagesSnapshot()
	if !decision.AllowRecentMemory {
		recentMessages = nil
	}
	bundle, err := resolver.Resolve(ctx, memory.Request{
		ChannelID:      ps.ctx.ChatID,
		TopicID:        ps.ctx.ThreadID,
		ProjectDir:     ps.ctx.ProjectDir,
		UserMessage:    userMessage,
		Mode:           mode,
		BudgetChars:    4000,
		RecentMessages: recentMessages,
	})
	if err != nil {
		log.Printf("[agent] memory bridge resolve failed mode=%s: %v", mode, err)
		return ""
	}
	return bundle.Render()
}

func (a *Agent) callStreamWithResumeBridge(
	ctx context.Context,
	ps *projectState,
	message string,
	backend engine.BackendKind,
	sessionID, model string,
	onToolUse func(string, map[string]interface{}),
	onContent func(string, string),
) (*client.CLIResponse, string, string, error) {
	resp, err := a.client.CallStream(ctx, message, a.projectDir, sessionID, model, onToolUse, onContent)
	if err == nil || sessionID == "" || !errors.Is(err, client.ErrSessionUnavailable) {
		return resp, message, sessionID, err
	}

	if ps != nil && ps.ctx != nil {
		ps.ctx.ClearSession(backend)
	}

	bridge := ""
	if ps != nil && ps.ctx != nil && !a.suppressMemoryBridge {
		bridge = a.resolveMemoryBridge(ctx, ps, message, "direct_resume_fallback")
	}
	if bridge == "" {
		log.Printf("[agent] session %q unavailable; retrying fresh (no bridge)", sessionID)
		resp, err = a.client.CallStream(ctx, message, a.projectDir, "", model, onToolUse, onContent)
		return resp, message, "", err
	}
	bridgedMessage := bridge + message
	log.Printf("[agent] session %q unavailable; bridge injected (%d recent messages)", sessionID, len(ps.ctx.RecentMsgs))
	resp, err = a.client.CallStream(ctx, bridgedMessage, a.projectDir, "", model, onToolUse, onContent)
	return resp, bridgedMessage, "", err
}

// ---------------------------------------------------------------------------
// Decision logging
// ---------------------------------------------------------------------------

func (a *Agent) logDecision(userPrompt, agentResponse string, toolCalls []ToolExecution, startTime time.Time, resp *client.CLIResponse, err error, routingReason string, routingLatency int, deltaCost float64) {
	if !a.decLog.enabled {
		return
	}
	userPrompt = a.filterPII(userPrompt)
	agentResponse = a.filterPII(agentResponse)

	duration := time.Since(startTime)
	ps := a.current()
	taskType := inferTaskType(userPrompt, toolCalls)
	filesChanged := extractFilesChanged(toolCalls)

	outcome := ExecutionOutcome{
		Success:      err == nil,
		TaskType:     taskType,
		FilesChanged: filesChanged,
		Summary:      generateSummary(userPrompt, agentResponse, taskType),
	}
	if err != nil {
		outcome.ErrorMessage = err.Error()
	}

	ctx := map[string]interface{}{
		"turns":       0,
		"project_dir": a.projectDir,
		"model":       a.client.GetModel(),
		"tool_count":  len(toolCalls),
		"has_error":   err != nil,
	}
	if resp != nil {
		ctx["turns"] = resp.NumTurns
	}

	tokenStats := TokenStats{Model: client.ExtractModelShortName(a.client.GetModel())}
	if resp != nil {
		tokenStats.TotalInputTokens = int64(resp.Usage.InputTokens)
		tokenStats.TotalOutputTokens = int64(resp.Usage.OutputTokens)
		tokenStats.TotalCostUSD = deltaCost
		tokenStats.APICallCount = 1
	}

	thinkingContent := ""
	if resp != nil {
		thinkingContent = resp.ThinkingContent
	}

	decision := DecisionLog{
		Timestamp:       startTime,
		SessionID:       ps.ctx.Session(ps.ctx.LastBackend),
		ProjectPath:     a.projectDir,
		ChannelID:       a.channelID,
		TopicID:         a.topicID,
		UserPrompt:      userPrompt,
		AgentResponse:   agentResponse,
		ThinkingContent: thinkingContent,
		ToolCalls:       toolCalls,
		Context:         ctx,
		Outcome:         outcome,
		DurationMs:      int(duration.Milliseconds()),
		TokensUsed:      tokenStats,
		Source:          "agent",
		Model:           client.ExtractModelShortName(a.lastUsedModel),
		RoutingReason:   routingReason,
		RoutingLatency:  routingLatency,
	}

	a.decLog.Log(decision)
	if a.events != nil {
		a.events.OnDecisionEvent(decision)
	}
	if a.storage != nil {
		go func(d DecisionLog) {
			if dbErr := a.storage.InsertDecisionLog(d); dbErr != nil {
				log.Printf("[agent] failed to persist decision log: %v", dbErr)
			}
		}(decision)
	}
}

func (a *Agent) filterPII(text string) string {
	if a.pii == nil {
		return text
	}
	filtered, _ := a.pii.DetectAndFilterPII(text, false, nil)
	return filtered
}

// ---------------------------------------------------------------------------
// Continuation / context bridge detection
// ---------------------------------------------------------------------------

func shouldInjectContinuationBridge(userMessage string) bool {
	if !isContinuationMessage(userMessage) {
		return false
	}
	_, hasIssue := memory.ParseIssueNumber(userMessage)
	return !hasIssue
}

func isContinuationMessage(message string) bool {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" || strings.Contains(trimmed, "```") {
		return false
	}
	runeCount := len([]rune(trimmed))
	lower := strings.ToLower(trimmed)

	optionReferencePatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(處理完|处理完|選|选|做|方案|選項|选项|option)\s*[a-z]\b`),
		regexp.MustCompile(`(?i)\b[a-z]\s*(的話|的话|這個|这个|那個|那个|方案|選項|选项)`),
	}
	for _, pattern := range optionReferencePatterns {
		if pattern.MatchString(lower) {
			return true
		}
	}
	for _, phrase := range []string{"第一個", "第一个", "第二個", "第二个", "第三個", "第三个", "前者", "後者", "后者"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	for _, prefix := range []string{
		"但是", "但", "那", "繼續", "继续", "還有", "还有", "所以",
		"另外", "接著", "然後", "再來", "而且", "不過", "可是",
		"but", "and", "also", "continue", "what about", "furthermore", "moreover", "then", "next", "additionally",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	for _, phrase := range []string{"那", "呢", "繼續", "继续", "還有", "还有"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	for _, phrase := range []string{"這個", "这个", "那個", "那个", "它", "這", "这", "這樣", "这样", "那樣", "那样", "這裡", "这里", "那裡", "那里"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	for _, word := range []string{"this", "that", "it", "them", "those", "these"} {
		if regexp.MustCompile(`\b`+regexp.QuoteMeta(word)+`\b`).MatchString(lower) {
			return true
		}
	}
	if runeCount < 30 {
		for _, prefix := range []string{"為什麼", "为什么", "怎麼", "怎么", "如何", "哪裡", "哪里", "什麼時候", "什么时候", "why", "how", "where", "when"} {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
		for _, phrase := range []string{"有沒有", "有没有", "是否", "是不是", "了嗎", "了吗", "了沒", "了没"} {
			if strings.Contains(lower, phrase) {
				return true
			}
		}
	}
	for _, word := range []string{
		"好", "是", "對", "行", "嗯", "去", "做", "試試",
		"好啊", "好的", "好了", "可以", "繼續", "繼續吧", "繼續做", "繼續進行", "請繼續",
		"修正", "下一步", "之後", "做吧", "沒問題",
		"ok", "yes", "y", "go", "sure", "continue", "proceed", "fix", "fix it", "next",
	} {
		if lower == word {
			return true
		}
	}
	return runeCount < 15
}

// ---------------------------------------------------------------------------
// Tool update formatting
// ---------------------------------------------------------------------------

func formatToolUpdate(name string, input map[string]interface{}) string {
	switch name {
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("Read %s", filepath.Base(path))
		}
		return "Read file"
	case "Write":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("Write %s", filepath.Base(path))
		}
		return "Write file"
	case "Edit":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("Edit %s", filepath.Base(path))
		}
		return "Edit file"
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			if len(cmd) > 60 {
				cmd = cmd[:60] + "..."
			}
			return fmt.Sprintf("$ %s", cmd)
		}
		return "Execute command"
	default:
		return fmt.Sprintf("[%s]", name)
	}
}

func processingStatusForModel(model string) string {
	switch engine.BackendKindForModel(model) {
	case engine.BackendCodex:
		return fmt.Sprintf("GPT/Codex processing with %s...", model)
	default:
		return fmt.Sprintf("Claude processing with %s...", model)
	}
}

func formatSkillsForPrompt(skills []AutoSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n[Auto-Skills]\n")
	for _, s := range skills {
		sb.WriteString(fmt.Sprintf("## %s\n%s\n\n", s.Name, s.Prompt))
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Task classification helpers (for DecisionLog)
// ---------------------------------------------------------------------------

func inferTaskType(userPrompt string, toolCalls []ToolExecution) string {
	prompt := strings.ToLower(userPrompt)
	switch {
	case strings.Contains(prompt, "read") || strings.Contains(prompt, "show") || strings.Contains(prompt, "what"):
		return "analysis"
	case strings.Contains(prompt, "write") || strings.Contains(prompt, "create") || strings.Contains(prompt, "add"):
		return "code_generation"
	case strings.Contains(prompt, "fix") || strings.Contains(prompt, "debug") || strings.Contains(prompt, "error"):
		return "debugging"
	case strings.Contains(prompt, "test"):
		return "testing"
	case strings.Contains(prompt, "commit") || strings.Contains(prompt, "git"):
		return "version_control"
	}
	hasFileOps, hasBash := false, false
	for _, tc := range toolCalls {
		switch tc.ToolName {
		case "Read", "Write", "Edit":
			hasFileOps = true
		case "Bash":
			hasBash = true
		}
	}
	if hasFileOps && hasBash {
		return "code_generation"
	}
	if hasFileOps {
		return "file_operation"
	}
	if hasBash {
		return "command_execution"
	}
	return "analysis"
}

func extractFilesChanged(toolCalls []ToolExecution) []string {
	seen := make(map[string]bool)
	var files []string
	for _, tc := range toolCalls {
		if tc.ToolName == "Write" || tc.ToolName == "Edit" {
			if path, ok := tc.Input["file_path"].(string); ok && !seen[path] {
				seen[path] = true
				files = append(files, path)
			}
		}
	}
	return files
}

func generateSummary(userPrompt, agentResponse, taskType string) string {
	promptPreview := userPrompt
	if len(promptPreview) > 100 {
		promptPreview = promptPreview[:100] + "..."
	}
	return fmt.Sprintf("[%s] %s", taskType, promptPreview)
}

func addToRecentMessages(ps *projectState, userMsg, assistantMsg string) {
	if ps == nil || ps.ctx == nil {
		return
	}
	ps.ctx.AddRecentMessage(userMsg, assistantMsg)
}
