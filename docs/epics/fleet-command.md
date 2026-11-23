# Fleet 指揮系統：Epic 與開發 Issues

狀態：草案，待確認後建立為 GitHub issues。

## 1. 背景與決策

Alice 目前以 `hermes` 把一個目標拆成子任務再逐一執行與審查。實際使用上拆解經常卡住，而現在的模型已內建任務內的規劃能力。因此：

- **放棄**：自研的任務拆解（planner、reducer、snapshot、replan、graph walker、逐子任務 reviewer）。
- **新增**：跨專案的指揮層 `fleet`，設計上致敬 [firstmate](https://github.com/kunchenguid/firstmate)：一個任務交給一個 agent 在獨立 worktree 做到完，指揮層只負責派工、隔離、監督、驗收、通知。
- **介面分工**：Telegram 是受限介面，做「派工、看摘要、做決策」；網頁端不受限，做完整的指揮中心。兩者都是同一套 Command API 的薄客戶端。

### 核心模型

| 概念 | 定義 |
|---|---|
| Project | 一個受管控的 repo：路徑、預設分支、信任模式、驗收指令、並行上限、TG topic 綁定 |
| Task | 一次 agent 執行，從頭做到完，沒有子任務 |
| Shape | `ship`（交付變更）或 `scout`（只產出報告，禁止寫入與 push） |
| Trust mode | `no-mistakes`（驗收通過且人工核准才合併）、`direct-pr`（驗收通過自動開 PR）、`local-only`（只在本地分支，不 push） |
| Worktree | 每個 ship 任務一個獨立 worktree 與分支（預設前綴 `alice/`） |

任務狀態：`queued → running → verifying → review_ready → done`，另有 `failed`、`needs_decision`、`cancelled`。

存活狀態與任務狀態分開記錄：`busy / idle / unknown / dead`。缺少或過期的訊號一律視為 `unknown`，不可視為 `idle`。

### 設計原則

1. 指揮層不呼叫模型。監督、分類、驗收全部是確定性的 Go 程式。
2. 所有狀態落在 SQLite，程序重啟後可接續。
3. 驗收以實際執行 build／test 的結束碼為準，不採信 agent 的自述。
4. 髒的 worktree 拒絕回收。
5. 只在「需要人處理」時通知。

### Repo 歸屬

- `alice-core`：`fleet/` 套件、Command API 的 Go 介面與事件型別、儲存。
- `alice`：Telegram 介面、HTTP／WebSocket 轉接、網頁前端。

下列 issue 以暫時編號標示，建立到 GitHub 後替換為實際編號。規模：S 約半天，M 約一到兩天，L 約三到五天。

---

## Epic A：解耦並凍結 hermes（alice-core）

目標：在不破壞上游 Alice 的前提下，把要保留的資產從 `hermes` 搬出，讓 `agent`、`memory`、`storage`、`issueops` 不再依賴 planner 相關型別。

### A-1 補上基準測試與 CI（M）
整個 repo 目前沒有測試。重構前先建立安全網。
- [ ] 新增 GitHub Actions：`go build ./...`、`go vet ./...`、`go test ./...`
- [ ] `client`：以假的 `claude` 執行檔測試參數組裝與 stream-json 解析
- [ ] `process`：逾時、取消、輸出截斷
- [ ] `hermes/github.go`：checklist 解析與回寫的表格式測試
- [ ] 打 tag `v0.1.0-hermes`，作為上游可回退的版本

### A-2 抽出 `ghissue/` 套件（M）
把 `hermes/github.go`、`hermes/github_config.go`、`hermes/issue_state.go` 搬到獨立套件。
- [ ] 新套件不 import `hermes`
- [ ] `IssueContext`、`ChecklistItem`、`IssueState` 及轉移規則移入
- [ ] `hermes` 內保留 type alias 與轉呼叫，上游編譯不受影響
- [ ] 依賴：A-1

### A-3 `issueops` 改依賴 `ghissue`（M）
- [ ] 把 `hermes.SubTask` 的使用換成 `issueops` 自有的 `WorkItem{ID, Description}`
- [ ] `issueops` 不再 import `hermes`
- [ ] checklist 對應與 evidence 行為以測試鎖住
- [ ] 依賴：A-2

### A-4 抽出 `chat/` 套件，解開 `agent → engine`（M）
`agent` 只用到 `ChatContext`、`ExecutionLifecycle`、`DecideRecovery`、`errorclass`，卻因此連帶依賴整個 `engine` 與 `hermes`。
- [ ] `ChatContext`、`ContextMessage`、`BackendKind`、session policy 移到 `chat/`
- [ ] `ExecutionLifecycle`、recovery policy 移到 `agent/` 或 `chat/`
- [ ] `agent`、`memory` 不再 import `engine`
- [ ] 依賴：A-1

### A-5 抽出 `verify/` 套件（S）
把 `hermes/hooks.go` 的 `PathGuard`、`GoBuild`、`TscCheck`、`JsonLint` 搬出，改成可重用的驗收步驟。
- [ ] 介面：`Step.Run(ctx, dir) Result{Passed, Command, Output, Duration}`
- [ ] 支援任意 shell 指令步驟
- [ ] 依賴：A-1

### A-6 `memory` 以介面取代 `hermes.TaskState`（S）
- [ ] 定義 `memory.TaskSummary` 與 `TaskSource` 介面
- [ ] `hermes` 與之後的 `fleet` 各提供 adapter
- [ ] `memory` 不再 import `hermes`
- [ ] 依賴：A-4

---

## Epic B：Fleet 核心（alice-core）

目標：能登記多個專案，並對每個專案平行派發任務，任務在隔離的 worktree 中執行到完成、通過驗收。

### B-1 Project registry（M）
- [ ] `fleet.Project`：ID、名稱、repo 路徑、remote、預設分支、信任模式、驗收指令、並行上限、分支前綴
- [ ] SQLite 表 `fleet_projects`，CRUD 與測試
- [ ] 登記時檢查：路徑是 git repo、預設分支存在
- [ ] 專案可附加任意 key-value 綁定（供 TG topic 等使用）
- [ ] 依賴：A-1

### B-2 Task 模型與狀態機（M）
- [ ] `fleet.Task`：ID、專案、shape、目標文字、來源（手動或 issue 編號）、狀態、分支、worktree 路徑、session ID、token 與成本、時間戳
- [ ] 合法轉移表與 `Transition()`，非法轉移回傳錯誤
- [ ] SQLite 表 `fleet_tasks`、`fleet_task_events`（只增不改）
- [ ] 每個轉移都有表格式測試
- [ ] 依賴：B-1

### B-3 `worktree/` 套件（M）
- [ ] `Create(repo, branch, base)`、`Remove(path)`、`List(repo)`、`IsDirty(path)`
- [ ] `Remove` 遇到髒的 worktree 回傳 `ErrDirty`，需明確傳入 force
- [ ] worktree 集中放在可設定的根目錄下，路徑含專案與任務 ID
- [ ] 以暫時 repo 做整合測試
- [ ] 依賴：A-1

### B-4 Runner：一個任務一個 agent 程序（L）
- [ ] 在任務的 worktree 內啟動 `claude`，串流事件寫入 `fleet_task_events`
- [ ] 以 `--resume` 支援中斷後接續與追加指示
- [ ] 取消：先送中斷，逾時後強制結束
- [ ] 記錄 token、成本、最後活動時間
- [ ] scout 任務以工具白名單限制為唯讀，並要求輸出 `report.md`
- [ ] 以假的 `claude` 執行檔測試完整生命週期
- [ ] 依賴：B-2、B-3

### B-5 Scheduler 與並行控制（M）
- [ ] 全域與每專案的並行上限
- [ ] 佇列依優先序與建立時間出隊
- [ ] 同一分支不可同時有兩個執行中的任務
- [ ] 依賴：B-4

### B-6 Watcher：零 token 監督（L）
- [ ] 單一 goroutine 監看所有執行中的任務
- [ ] 以程序狀態、最後事件時間、輸出內容推導 `busy / idle / unknown / dead`
- [ ] 分類需要處理的事件：完成、失敗、卡住（超過門檻無活動）、agent 提問、超出預算
- [ ] 可處理事件寫入持久的 `fleet_wakes` 佇列，消費後才標記
- [ ] 訊號缺失時判為 `unknown` 的測試
- [ ] 依賴：B-4

### B-7 驗收閘門（M）
- [ ] 任務進入 `verifying` 時依序執行專案的驗收指令
- [ ] 結果（指令、結束碼、輸出尾段、耗時）存入 `fleet_verifications`
- [ ] 失敗時把輸出回饋給同一個 session 重試，次數可設定，用完轉 `needs_decision`
- [ ] scout 任務的驗收為：`report.md` 存在且 worktree 無變更
- [ ] 依賴：A-5、B-4

### B-8 信任模式與交付（L）
- [ ] `local-only`：驗收通過後停在 `review_ready`，不 push
- [ ] `direct-pr`：push 分支並開 PR，記錄 PR 連結
- [ ] `no-mistakes`：開 PR 後等待人工核准，核准後才合併
- [ ] 合併只走 fast-forward 或 PR 合併，不改寫歷史
- [ ] 模式在派工時固定，執行中不可變更
- [ ] 依賴：B-7

### B-9 重啟復原（M）
- [ ] 啟動時掃描非終態任務，與實際程序、worktree 對帳
- [ ] 程序已不存在的 `running` 任務標為 `dead`，轉 `needs_decision`
- [ ] 孤兒 worktree 列出報告，不自動刪除
- [ ] 依賴：B-6

### B-10 預算與配額（S）
- [ ] 每任務、每專案、每日的 token 與成本上限
- [ ] 超出時暫停任務並產生 wake 事件
- [ ] 依賴：B-6

### B-11 GitHub issue 作為任務來源（M）
- [ ] 以 issue 編號派工，issue 內容與未勾選的 checklist 帶入目標
- [ ] 任務開始、完成、失敗時在 issue 留言
- [ ] 驗收通過後同步 checklist，並附上驗收證據
- [ ] 依賴：A-3、B-7

---

## Epic C：Command API（alice-core）

目標：TG 與網頁共用的單一指揮介面。兩個前端都不直接碰 `fleet` 的內部。

### C-1 `fleet.Commander` 介面（M）
- [ ] 指令：`RegisterProject`、`UpdateProject`、`Dispatch`、`Cancel`、`Retry`、`Reply`（回答 agent 提問或追加指示）、`Approve`、`Merge`、`Discard`
- [ ] 查詢：`ListProjects`、`ListTasks`（可依專案、狀態篩選）、`GetTask`、`TaskEvents`（分頁）、`TaskDiff`、`Digest`
- [ ] 所有指令帶上操作者身分並寫入稽核紀錄
- [ ] 依賴：B-2

### C-2 事件匯流排（M）
- [ ] `Subscribe(filter)` 回傳事件 channel：任務狀態變更、存活狀態變更、驗收結果、wake
- [ ] 支援從指定事件 ID 之後重播，供斷線重連
- [ ] 與既有的 `agent.EventSink` 相容
- [ ] 依賴：B-6

### C-3 通知政策（M）
- [ ] 每位使用者的模式：`normal`、`quiet`（只收需要決策的）、`afk`（在授權範圍內自動處理，回來時給摘要）
- [ ] `afk` 的授權範圍明確列舉且有到期時間，到期自動回到 `normal`
- [ ] 同一任務的通知合併，設最短間隔
- [ ] 依賴：C-2

### C-4 Digest 產生器（S）
- [ ] 艦隊摘要：各專案執行中、待審、待決策、失敗的數量，以及今日成本
- [ ] 輸出結構化資料，由前端各自排版
- [ ] 依賴：C-1

---

## Epic D：Telegram 指揮介面（alice）

目標：在手機上完成派工、掌握狀況、做決策。不在 TG 內呈現長篇 log 或完整 diff，改以連結導向網頁。

### D-1 Topic 與專案綁定（M）
- [ ] 一個 topic 對應一個專案，在該 topic 內的訊息預設派到該專案
- [ ] `/bind`、`/unbind`，以及未綁定 topic 的提示
- [ ] General topic 作為艦隊總覽頻道
- [ ] 依賴：B-1、C-1

### D-2 派工指令（M）
- [ ] 直接輸入文字即派發 ship 任務
- [ ] `/scout <問題>` 派發調查任務
- [ ] `/issue <編號>` 以 GitHub issue 派工
- [ ] 回覆確認訊息含任務 ID 與取消按鈕
- [ ] 依賴：D-1、B-11

### D-3 任務卡片（M）
- [ ] 每個任務一則訊息，狀態變更時原地編輯，不洗版
- [ ] 顯示：狀態、已執行時間、目前動作的一行摘要、成本
- [ ] 按鈕依狀態變化：取消、重試、看詳情（開網頁）、核准、合併、捨棄
- [ ] 依賴：C-2

### D-4 決策提示（M）
- [ ] agent 提問、驗收用盡重試、超出預算、任務卡住時發出提示
- [ ] 常見選項以按鈕呈現，也可直接回覆文字
- [ ] 回覆該訊息即送入對應任務的 session
- [ ] 依賴：D-3

### D-5 艦隊總覽指令（S）
- [ ] `/fleet`：跨專案摘要
- [ ] `/projects`：專案清單與各自狀態
- [ ] `/tasks`：本 topic 專案的任務清單
- [ ] 依賴：C-4

### D-6 通知模式（S）
- [ ] `/quiet`、`/afk <時長> <授權範圍>`、`/back`
- [ ] `/back` 時回傳離開期間的摘要
- [ ] 依賴：C-3

### D-7 交付結果訊息（S）
- [ ] 完成時送出：變更檔案數、增刪行數、驗收結果、PR 連結
- [ ] scout 任務送出報告摘要，並以檔案附上 `report.md`
- [ ] 依賴：D-3

---

## Epic E：Web 指揮中心（alice）

目標：完整的艦隊管控介面，涵蓋 TG 放不下的所有內容。

### E-1 HTTP 與 WebSocket 轉接層（M）
- [ ] 把 `Commander` 的指令與查詢對應成 REST 端點
- [ ] WebSocket 推送事件，支援以事件 ID 續傳
- [ ] 沿用既有 dashboard 的登入機制
- [ ] 依賴：C-1、C-2

### E-2 艦隊看板（L）
- [ ] 以狀態分欄的看板，橫跨所有專案，可依專案篩選
- [ ] 卡片即時更新，存活狀態以顏色區分，`unknown` 有獨立樣式
- [ ] 頂部顯示全域計數與今日成本
- [ ] 依賴：E-1

### E-3 任務詳情頁（L）
- [ ] 即時串流的事件時間軸：工具呼叫、訊息、狀態轉移
- [ ] Diff 檢視：以檔案分組，可切換並排與統一格式
- [ ] 驗收結果：每個指令的輸出與結束碼
- [ ] 操作列：取消、重試、追加指示、核准、合併、捨棄
- [ ] 依賴：E-1

### E-4 派工編輯器（M）
- [ ] 選擇專案、shape、模型、預算上限
- [ ] 批次派工：一次輸入多個任務
- [ ] 從 GitHub issue 清單勾選派工
- [ ] 依賴：E-1、B-11

### E-5 決策收件匣（M）
- [ ] 集中列出所有 `needs_decision` 與 `review_ready` 的任務
- [ ] 每一項附上情境與可選動作，不必進入詳情頁即可處理
- [ ] 依賴：E-1

### E-6 專案管理頁（M）
- [ ] 登記與編輯專案：路徑、信任模式、驗收指令、並行上限
- [ ] 驗收指令可試跑並顯示結果
- [ ] 顯示該專案的 worktree 清單，標示髒的與孤兒的
- [ ] 依賴：E-1

### E-7 成本與用量儀表板（M）
- [ ] 依專案、依日期、依模型的 token 與成本
- [ ] 預算使用率與超限紀錄
- [ ] 依賴：E-1、B-10

### E-8 稽核紀錄（S）
- [ ] 所有指令的操作者、時間、來源（TG 或網頁）、結果
- [ ] 可依專案與任務篩選
- [ ] 依賴：E-1

---

## Epic F：切換與移除 hermes

### F-1 Alice 改走 fleet（alice，L）
- [ ] 以設定開關在 hermes 與 fleet 之間切換，預設仍為 hermes
- [ ] 在至少兩個真實專案上以 fleet 完成十個任務並記錄結果
- [ ] 預設值切到 fleet
- [ ] 依賴：Epic B、C、D

### F-2 舊資料處理（alice-core，S）
- [ ] 舊的 `hermes_*` 表保留唯讀，不做遷移
- [ ] 提供匯出歷史任務摘要的指令
- [ ] 依賴：F-1

### F-3 移除 hermes 與 engine 的 planner 程式碼（alice-core，M）
- [ ] 刪除 `hermes/` 中的 planner、reducer、snapshot、store、accumulated、progress、summary
- [ ] 刪除 `engine/graph/`、`engine/plan_execute.go`、`engine/graph_bridge.go`、`engine/review.go`、`engine/task_retry.go`
- [ ] 移除 A-2 留下的 alias
- [ ] 更新 README，打 tag `v0.2.0`
- [ ] 依賴：F-1、F-2

---

## 建議順序

| 階段 | 內容 | 產出 |
|---|---|---|
| 1 | A-1 至 A-6 | 套件解耦完成，有測試與 CI |
| 2 | B-1 至 B-7、C-1、C-2 | 可用 Go API 對多個專案平行派工並驗收 |
| 3 | D-1 至 D-4、B-8、B-9 | TG 上可派工、看狀態、做決策、交付 |
| 4 | E-1 至 E-3、E-5 | 網頁看板、任務詳情、決策收件匣 |
| 5 | 其餘 B、C、D、E 項目 | 預算、通知模式、成本、稽核 |
| 6 | F-1 至 F-3 | 移除 hermes |

階段 1 與階段 2 的 B-1 至 B-3 沒有相依，可以同時進行。

## 待確認事項

1. TG 與網頁的 issue 是否建立在 `alice` repo。本文件撰寫時無法讀取該 repo，Epic D、E 中「沿用既有機制」的部分需對照實際程式碼調整。
2. `no-mistakes` 模式的核准者是否只有你一人。若是，核准流程可以簡化為單一按鈕。
3. worktree 根目錄與磁碟配額的預設值。
