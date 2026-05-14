# alice-core

Reusable Go library extracted from [Alice](https://github.com/chimerakang/alice) — the AI coding agent that wraps Claude Code CLI as a Telegram bot.

`alice-core` provides the full agent execution stack as an importable module so other projects can embed Claude Code as an AI coding agent without any Telegram or dashboard dependencies.

## Packages

| Package | Description |
|---------|-------------|
| `client/` | `CLIClient` wrapping the `claude` CLI subprocess, `EnhancedCLIClient` with file attachment support, `Client` interface |
| `agent/` | `Agent` struct with `Run`/`Abort`/`Reset`, dynamic model routing, sticky sessions, `ToolLogger`, `DecisionLogger`, injected dependency interfaces |
| `memory/` | `UnifiedMemoryResolver` — context bridge across model/session switches, `ProjectStaticHintSource` reading CLAUDE.md |
| `git/` | `GitInfo` (branch, commit hash, dirty state) with 30-second TTL cache |
| `storage/` | `SQLiteStorage` implementing `StorageBackend` (tool executions, decision logs, runtime events) + unified task store |
| `engine/` | `ChatContext`, plan-execute FSM, session policy, recovery policy, review engine, graph walker |
| `hermes/` | Planner/executor system, `SQLiteTaskStore`, task state machine, snapshot |
| `issueops/` | GitHub issue checklist mapping, evidence, service |
| `process/` | External command runner (`RunOutput`, `Command`) |

## Requirements

- Go 1.24+
- `claude` CLI installed and authenticated (`claude auth`)

## Installation

```bash
go get github.com/chimerakang/alice-core
```

## Quick Start

```go
package main

import (
    "fmt"

    "github.com/chimerakang/alice-core/agent"
    "github.com/chimerakang/alice-core/client"
    "github.com/chimerakang/alice-core/storage"
)

func main() {
    // SQLite storage (optional — pass nil Options to skip persistence)
    store, err := storage.NewSQLiteStorage("./data/alice.db")
    if err != nil {
        panic(err)
    }
    defer store.Close()

    // Claude CLI client
    cli := client.NewCLIClient("claude-sonnet-4-6")

    // Create agent for a project directory
    ag := agent.NewAgentWithOptions(cli, "/path/to/project", 1, 0, agent.Options{
        Storage: store,
    })

    // Run a task — onUpdate receives streaming progress messages
    result, err := ag.Run("重構這個函式，加上錯誤處理", func(msg string, silent bool) {
        if !silent {
            fmt.Println(msg)
        }
    })
    if err != nil {
        panic(err)
    }
    fmt.Println(result)
}
```

## Dependency Injection

All external concerns are injected via `agent.Options`. Nil values fall back to safe no-ops.

```go
ag := agent.NewAgentWithOptions(cli, projectDir, channelID, topicID, agent.Options{
    Storage:         store,          // agent.StorageBackend — persist tool/decision logs
    Events:          myHub,          // agent.EventSink — broadcast to WebSocket / UI
    Metrics:         myPrometheus,   // agent.MetricsRecorder — record latency/token metrics
    PIIFilter:       myFilter,       // agent.PIIFilter — scrub sensitive data before logging
    Skills:          mySkillMatcher, // agent.SkillMatcher — auto-inject relevant skill prompts
    DecisionLogSize: 100,
    ToolLogSize:     200,
})
```

### Implementing an interface

```go
// Example: minimal EventSink that logs to stdout
type StdoutSink struct{}

func (s *StdoutSink) OnToolEvent(eventType string, exec agent.ToolExecution) {
    fmt.Printf("[tool] %s %s\n", eventType, exec.ToolName)
}

func (s *StdoutSink) OnDecisionEvent(decision agent.DecisionLog) {
    fmt.Printf("[decision] %s success=%v\n", decision.Model, decision.Outcome.Success)
}
```

## Model Routing

The agent selects models automatically based on message content using `DefaultModelRoutes()`. You can override at any time:

```go
// Force a specific model for the next run
ag.SetModelOverride("claude-opus-4-7")

// Enable two-phase plan/execute
ag.SetPlanMode(true, "claude-opus-4-7", "claude-sonnet-4-6")

// Apply sticky-session routing from config
ag.SetRoutingConfig(agent.ModelRoutingConfig{
    EnableDynamicRouting: true,
    StickySession:        true,
    SessionIdleTimeoutMin: 60,
})
```

## Hermes Planner (multi-step tasks)

`hermes` provides a full plan-execute system for decomposing complex tasks into sub-tasks with review:

```go
import (
    "github.com/chimerakang/alice-core/hermes"
    "github.com/chimerakang/alice-core/engine"
)

store := hermes.NewSQLiteTaskStore("./data/alice.db")
runner := hermes.NewProcessRunner(cli, projectDir, channelID, topicID)
planner := hermes.NewPlanner(store, runner, hermes.DefaultConfig())

ctx := context.Background()
chatCtx := engine.NewChatContext(channelID, topicID, projectDir)
result, err := planner.Run(ctx, "實作一個 REST API，包含 CRUD 操作", chatCtx)
```

## Git Integration

```go
import "github.com/chimerakang/alice-core/git"

info, err := git.Get("/path/to/project")
if err == nil {
    fmt.Printf("branch=%s commit=%s dirty=%v\n", info.Branch, info.CommitHash, info.IsDirty)
}
```

## Architecture

```
Your App
    │
    ├── agent.Agent.Run(message, onUpdate)
    │       │
    │       ├── client.CLIClient  ←──── claude CLI subprocess
    │       ├── memory.UnifiedMemoryResolver  (context bridge)
    │       ├── agent.StorageBackend  (your SQLiteStorage or custom)
    │       └── agent.EventSink  (your WebSocket hub or custom)
    │
    └── hermes.Planner  (optional multi-step execution)
            │
            ├── engine.PlanExecuteEngine
            ├── engine.graph.Walker  (node-based FSM)
            └── hermes.SQLiteTaskStore
```

## License

MIT
