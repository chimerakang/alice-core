# alice-core

Reusable Go library extracted from [Alice](https://github.com/chimerakang/alice) — the AI coding agent that wraps Claude Code CLI as a Telegram bot.

`alice-core` provides the full agent execution stack as an importable module so other projects can embed Claude Code as an AI coding agent without any Telegram or dashboard dependencies.

## Packages

| Package | Description |
|---------|-------------|
| `client/` | `CLIClient` wrapping the `claude` CLI subprocess, `EnhancedCLIClient` with file attachment support, `Client` interface |
| `agent/` | `Agent` struct with `Run`/`Abort`/`Reset`, dynamic model routing, sticky sessions, `ToolLogger`, `DecisionLogger`, injected dependency interfaces |
| `memory/` | `UnifiedMemoryResolver` — context bridge across model/session switches, `ProjectStaticHintSource` reading CLAUDE.md, `TaskSource` interface for orchestrator task history |
| `git/` | `GitInfo` (branch, commit hash, dirty state) with 30-second TTL cache |
| `storage/` | `SQLiteStorage` implementing `StorageBackend` (tool executions, decision logs, runtime events) + unified task store |
| `chat/` | `ChatContext` and its lifecycle, native session policy, `ExecutionLifecycle`, error recovery policy. No orchestrator dependency |
| `errorclass/` | Classifies error text as rate limit, overload, network reset, timeout, EOF, context overflow, or killed |
| `ghissue/` | GitHub issues via the `gh` CLI: fetch, checklist parsing and write-back, comments, labels, issue lifecycle FSM |
| `fleet/` | Command layer for running agents across projects: project registry, task model and lifecycle state machine, append-only task event log, a `Runner` (one agent per task in its own worktree), a `Verifier` (acceptance by exit code), a `Scheduler` (queue and concurrency limits), a `Deliverer` (commit, push, pull request, approval), a `Watcher` (zero-token supervision with a durable wake queue), and the operating surface: `Commander` (audited commands and queries), `Bus` (resumable event stream) and `Notifier` (normal / quiet / afk) |
| `worktree/` | Per-task git worktrees: create, attach, list, prune, and removal that refuses uncommitted work |
| `verify/` | Acceptance checks judged by exit code: `Step` interface, shell-command steps, `go build` / `tsc` / `jq` steps, path deny-list guard |
| `engine/` | Plan-execute FSM, recovery policy for plan-execute modes, review engine, graph walker. Re-exports `chat` types for compatibility |
| `hermes/` | Planner/executor system, `SQLiteTaskStore`, task state machine, snapshot. Re-exports `ghissue` types for compatibility |
| `issueops/` | GitHub issue workflow over orchestrator-neutral `WorkItem`s: checklist mapping, evidence, guarded sync and close |
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

## Fleet (multi-project task tracking)

`fleet` records which repositories are under management and every task dispatched to them. One task is one agent run from start to finish; nothing is decomposed.

```go
import (
    "github.com/chimerakang/alice-core/fleet"
    "github.com/chimerakang/alice-core/worktree"
)

store, err := fleet.OpenStore("./data/alice.db") // or fleet.NewStore(db) to share a *sql.DB
defer store.Close()

// Register a repository. Remote and default branch are read from it.
project, err := store.RegisterProject(ctx, fleet.Project{
    RepoPath:       "/path/to/project",
    TrustMode:      fleet.TrustLocalOnly, // or TrustDirectPR, TrustNoMistakes
    VerifyCommands: []string{"go build ./...", "go test ./..."},
})

// Dispatch a task and give it an isolated checkout.
task, err := store.CreateTask(ctx, fleet.NewTask{ProjectID: project.ID, Goal: "修好不穩定的登入測試"})
path, _ := worktree.Layout{Root: "./data/worktrees"}.Path(project.ID, task.ID)
branch := project.BranchName(task.ID)
_, err = worktree.Create(ctx, project.RepoPath, path, branch, project.DefaultBranch)
err = store.SetWorkspace(ctx, task.ID, branch, path)

// Every status change is validated and written to the task's event log.
task, err = store.Transition(ctx, task.ID, fleet.StatusRunning, "slot free")
```

`fleet.Runner` runs a task's agent to the end of one turn and leaves the task in the status that describes how it ended:

```go
runner := &fleet.Runner{
    Store:      store,
    Worktrees:  worktree.Layout{Root: "./data/worktrees"},
    ReportRoot: "./data/reports", // scout reports land here
}
res, err := runner.Run(ctx, task.ID, fleet.RunOptions{}) // blocks; creates the worktree if needed
// res.Outcome: finished | needs_decision | failed | cancelled

runner.Interrupt(task.ID, "wrong approach")                              // stop, keep the session
runner.Run(ctx, task.ID, fleet.RunOptions{Instruction: "try it this way"}) // resume with instructions
runner.Cancel(task.ID, "not needed")                                     // interrupt, then kill after StopGrace
```

A `ShapeScout` task gets only read-only tools; its final message is saved as `report.md`.

`fleet.Scheduler` drains the queue within the concurrency limits and carries each task through run and verification:

```go
sched := &fleet.Scheduler{Store: store, Runner: runner, MaxConcurrent: 2}
go sched.Run(ctx) // starts queued tasks as slots open; stops their agents when ctx ends

store.CreateTask(ctx, fleet.NewTask{ProjectID: project.ID, Goal: "...", Priority: 5})
sched.Kick() // look at the queue now instead of at the next poll
```

After the agent finishes, `fleet.Verifier` runs the project's `VerifyCommands` in the task's worktree and judges by exit code alone. A failure is sent back to the same agent session with the failing command and its output, up to `MaxRetries` times, then the task becomes `needs_decision`. A scout is checked for a non-empty report and an untouched checkout. Every check is stored in `fleet_verifications`.

A verified ship task is then delivered as far as its trust mode allows. Set `Scheduler.Deliverer` to have this happen automatically:

| Trust mode | After verification | `Deliverer.Approve` |
|---|---|---|
| `local-only` | work committed on its branch; nothing pushed; task waits | fast-forwards the default branch |
| `direct-pr` | branch pushed, pull request opened, task done | not applicable |
| `no-mistakes` | branch pushed, pull request opened, task waits | merges the pull request |

History is never rewritten: a plain push, then a fast-forward or a merge commit. A task that changed nothing, or whose changes include a secret file (`.env`, `*.pem`, `*.key`, SSH keys), is stopped and handed to the owner instead.

`Runner.Budget` sets spending limits on tokens and cost in three scopes: one task's lifetime total, one project's day, and the whole fleet's day. A project can carry its own `Budget` to override the first two. Usage is known when an agent's turn ends, so a limit takes effect at the next boundary: no new run starts past it (`RunOptions.IgnoreBudget` is the owner's override), queued tasks wait, and when a daily limit is reached the runs already going are interrupted with their sessions kept.

`fleet.IssueTracker` dispatches tasks from GitHub issues and keeps each issue informed:

```go
tracker := &fleet.IssueTracker{Store: store}
sched.Issues = tracker

task, err := tracker.Dispatch(ctx, project.ID, 42, fleet.DispatchOptions{}) // goal built from the issue and its open checklist
```

The issue gets a comment when work starts and when it ends. A checklist item is ticked only when the agent declared it complete *and* the task passed verification with at least one real check; neither alone is enough.

`fleet.Watcher` supervises without calling a model. It turns status changes into wakes (review ready, completed, failed, question, decision needed), reports running tasks that have gone quiet or over budget, and keeps each task's `Liveness` honest: with no evidence either way it is `unknown`, never `idle`. Wakes are durable: read them with `Store.PendingWakes` and acknowledge each with `Store.ConsumeWake` once it has been acted on.

`Scheduler.Run` begins with `Runner.Recover`, which reconciles stored state with reality after a crash or restart: a task stored as running with no process behind it is marked dead and moved to `needs_decision` with its session intact, an agent process that outlived the host is stopped, and checkouts that no unfinished task owns are reported (never deleted). Set `Scheduler.OnRecovery` to receive the report.

### Operating the fleet

`fleet.Commander` is the single entry point for a chat bot, a web handler or a script. Every command names who gave it and is written to the audit log, refusals included.

```go
cmd := &fleet.Commander{Store: store, Scheduler: sched}
me := fleet.Actor{ID: "ck", Via: "telegram"}

task, err := cmd.Dispatch(ctx, me, fleet.DispatchRequest{ProjectID: "app", Goal: "修好登入測試"})
task, err = cmd.Dispatch(ctx, me, fleet.DispatchRequest{ProjectID: "app", Issue: 42})

cmd.Reply(ctx, me, fleet.ReplyRequest{TaskID: task.ID, Text: "用方案 B"}) // answers in the background
cmd.Approve(ctx, me, task.ID)  // land a review_ready task
cmd.Merge(ctx, me, task.ID)    // also merges the PR of a finished direct-pr task
cmd.Cancel(ctx, me, task.ID, "")
cmd.Retry(ctx, me, task.ID)
cmd.Discard(ctx, me, task.ID)  // cancel and remove the checkout; the branch is kept

diff, err := cmd.TaskDiff(ctx, task.ID)                     // committed and uncommitted work
events, next, more, err := cmd.TaskEvents(ctx, task.ID, 0, 50) // paged history
digest, err := cmd.Digest(ctx)                              // counts and today's usage per project
```

`fleet.Bus` streams events as they are stored. It is a tail on the durable event log, so a subscriber that reconnects with the last event ID it saw misses nothing and repeats nothing:

```go
bus := &fleet.Bus{Store: store}
events, err := bus.Subscribe(ctx, fleet.EventFilter{Types: fleet.LifecycleEvents, AfterID: lastSeen})
go bus.ForwardTools(ctx, myEventSink, fleet.EventFilter{}) // feeds an agent.EventSink
```

`fleet.Notifier` turns the wake queue into messages for the owner, merging everything pending about a task into one message and never messaging about a task more often than `MinInterval`. Three modes: `normal` delivers everything; `quiet` delivers only what cannot move without the owner and holds the rest; `afk` delivers nothing, acts only within an explicit grant (`approve`, `resume`, optionally limited to projects) that must carry an expiry, and hands over a summary on return or when the grant lapses.

```go
notifier := &fleet.Notifier{Store: store, Commander: cmd, User: "ck", Deliver: sendToTelegram}
go notifier.Run(ctx)

notifier.SetMode(ctx, fleet.NotifyAFK, &fleet.AFKGrant{
    Actions:   []fleet.AFKAction{fleet.AFKApprove},
    Projects:  []string{"app"},
    ExpiresAt: time.Now().Add(8 * time.Hour),
})
summary, err := notifier.Back(ctx) // what was handled and what was held
```

Task lifecycle: `queued → running → verifying → review_ready → done`, with `needs_decision`, `failed` and `cancelled` as the exits. `fleet.CanTransition` holds the full table; an illegal change returns a `*fleet.TransitionError`.

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
