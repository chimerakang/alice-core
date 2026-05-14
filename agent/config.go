package agent

import "encoding/json"

// ModelRoutingConfig controls dynamic model selection.
type ModelRoutingConfig struct {
	EnableDynamicRouting  bool   `json:"enable_dynamic_routing"`
	FastModel             string `json:"fast_model"`
	SmartModel            string `json:"smart_model"`
	DeepModel             string `json:"deep_model"`
	PlanModel             string `json:"plan_model"`
	ExecuteModel          string `json:"execute_model"`
	CodexFastModel        string `json:"codex_fast_model"`
	CodexSmartModel       string `json:"codex_smart_model"`
	CodexDeepModel        string `json:"codex_deep_model"`
	UseGPT4oMini          bool   `json:"use_gpt4o_mini_for_triage"`
	StickySession         bool   `json:"sticky_session"`
	SessionIdleTimeoutMin int    `json:"session_idle_timeout_min"`
	StickyMode            bool   `json:"sticky_mode"`
	SessionIdleTimeout    int    `json:"session_idle_timeout"`
}

// UnmarshalJSON accepts both sticky_session/session_idle_timeout_min and the
// sticky_mode/session_idle_timeout aliases.
func (c *ModelRoutingConfig) UnmarshalJSON(data []byte) error {
	type alias ModelRoutingConfig
	next := alias(*c)
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, ok := raw["sticky_mode"]; ok {
		next.StickySession = next.StickyMode
	} else if _, ok := raw["sticky_session"]; ok {
		next.StickyMode = next.StickySession
	}
	if _, ok := raw["session_idle_timeout"]; ok {
		next.SessionIdleTimeoutMin = next.SessionIdleTimeout
	} else if _, ok := raw["session_idle_timeout_min"]; ok {
		next.SessionIdleTimeout = next.SessionIdleTimeoutMin
	}
	*c = ModelRoutingConfig(next)
	c.Normalize()
	return nil
}

// Normalize resolves alias fields and applies defaults.
func (c *ModelRoutingConfig) Normalize() {
	if c.SessionIdleTimeout > 0 {
		c.SessionIdleTimeoutMin = c.SessionIdleTimeout
	} else if c.SessionIdleTimeoutMin > 0 {
		c.SessionIdleTimeout = c.SessionIdleTimeoutMin
	} else {
		c.SessionIdleTimeout = 1440
		c.SessionIdleTimeoutMin = 1440
	}
	if c.StickyMode || c.StickySession {
		c.StickyMode = true
		c.StickySession = true
	}
}

// StickyEnabled reports whether sticky-session mode is on.
func (c ModelRoutingConfig) StickyEnabled() bool {
	return c.StickyMode || c.StickySession
}

// IdleTimeoutMinutes returns the configured idle timeout in minutes.
func (c ModelRoutingConfig) IdleTimeoutMinutes() int {
	if c.SessionIdleTimeout > 0 {
		return c.SessionIdleTimeout
	}
	if c.SessionIdleTimeoutMin > 0 {
		return c.SessionIdleTimeoutMin
	}
	return 1440
}

// ModelRoute is a single keyword-to-model routing rule.
type ModelRoute struct {
	Pattern  string
	Model    string
	Priority int
}

// DefaultModelRoutes returns the built-in routing rules.
func DefaultModelRoutes() []ModelRoute {
	return []ModelRoute{
		{Pattern: `(?i)(翻譯|translat)`, Model: "haiku", Priority: 1},
		{Pattern: `(?i)(總結|summariz|摘要)`, Model: "haiku", Priority: 1},
		{Pattern: `(?i)(解釋|explain)`, Model: "haiku", Priority: 1},
		{Pattern: `(?i)(轉換格式|format|json|csv|xml)`, Model: "haiku", Priority: 2},
		{Pattern: `(?i)(讀取|查看|view|show|read|list|ls)`, Model: "haiku", Priority: 2},
		{Pattern: `(?i)(狀態|status)`, Model: "haiku", Priority: 2},
		{Pattern: `(?i)(改寫|改進|polish)`, Model: "haiku", Priority: 3},
		{Pattern: `(?i)(重構|refactor|架構|architecture)`, Model: "opus", Priority: 20},
		{Pattern: `(?i)(系統設計|design system)`, Model: "opus", Priority: 20},
		{Pattern: `(?i)(跨檔案|multiple files)`, Model: "opus", Priority: 21},
		{Pattern: `(?i)(bug修復|debug|診斷|troubleshoot)`, Model: "opus", Priority: 22},
		{Pattern: `(?i)(演算法|algorithm|邏輯設計)`, Model: "opus", Priority: 23},
		{Pattern: `(?i)(性能最佳化|optimiz|performance)`, Model: "opus", Priority: 24},
	}
}
