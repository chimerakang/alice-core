# Fleet 指揮系統：Epic 與開發 Issues

狀態：Epic A、B、C 與 F-2、F-3 已建立為 GitHub issues（標題後的 `#編號`）。Epic D、E 與 F-1、F-4、F-5、F-6 建立在 `chimerakang/alice`，標題後以 `alice#編號` 標示。

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
| Trust mode | `no-mistakes`（驗收通過且擁有者核准才合併）、`direct-pr`（驗收通過自動開 PR）、`local-only`（只在本地分支，不 push） |
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

下列 issue 保留設計編號（如 A-1），已建立者在標題後附上 GitHub 編號。規模：S 約半天，M 約一到兩天，L 約三到五天。

---

## Epic A：解耦並凍結 hermes（alice-core） #1

目標：在不破壞上游 Alice 的前提下，把要保留的資產從 `hermes` 搬出，讓 `agent`、`memory`、`storage`、`issueops` 不再依賴 planner 相關型別。

### A-1 補上基準測試與 CI（M） #5
整個 repo 目前沒有測試。重構前先建立安全網。
- [ ] 新增 GitHub Actions：`go build ./...`、`go vet ./...`、`go test ./...`
- [ ] `client`：以假的 `claude` 執行檔測試參數組裝與 stream-json 解析
- [ ] `process`：逾時、取消、輸出截斷
- [ ] `hermes/github.go`：checklist 解析與回寫的表格式測試
- [ ] 打 tag `v0.1.0-hermes`，作為上游可回退的版本

### A-2 抽出 `ghissue/` 套件（M） #6
把 `hermes/github.go`、`hermes/github_config.go`、`hermes/issue_state.go` 搬到獨立套件。
- [ ] 新套件不 import `hermes`
- [ ] `IssueContext`、`ChecklistItem`、`IssueState` 及轉移規則移入
- [ ] `hermes` 內保留 type alias 與轉呼叫，上游編譯不受影響
- [ ] 依賴：A-1

### A-3 `issueops` 改依賴 `ghissue`（M） #7
- [ ] 把 `hermes.SubTask` 的使用換成 `issueops` 自有的 `WorkItem{ID, Description}`
- [ ] `issueops` 不再 import `hermes`
- [ ] checklist 對應與 evidence 行為以測試鎖住
- [ ] 依賴：A-2

### A-4 抽出 `chat/` 套件，解開 `agent → engine`（M） #8
`agent` 只用到 `ChatContext`、`ExecutionLifecycle`、`DecideRecovery`、`errorclass`，卻因此連帶依賴整個 `engine` 與 `hermes`。
- [ ] `ChatContext`、`ContextMessage`、`BackendKind`、session policy 移到 `chat/`
- [ ] `ExecutionLifecycle`、recovery policy 移到 `agent/` 或 `chat/`
- [ ] `agent`、`memory` 不再 import `engine`
- [ ] 依賴：A-1

### A-5 抽出 `verify/` 套件（S） #9
把 `hermes/hooks.go` 的 `PathGuard`、`GoBuild`、`TscCheck`、`JsonLint` 搬出，改成可重用的驗收步驟。
- [ ] 介面：`Step.Run(ctx, dir) Result{Passed, Command, Output, Duration}`
- [ ] 支援任意 shell 指令步驟
- [ ] 依賴：A-1

### A-6 `memory` 以介面取代 `hermes.TaskState`（S） #10
- [ ] 定義 `memory.TaskSummary` 與 `TaskSource` 介面
- [ ] `hermes` 與之後的 `fleet` 各提供 adapter
- [ ] `memory` 不再 import `hermes`
- [ ] 依賴：A-4

---

## Epic B：Fleet 核心（alice-core） #2

目標：能登記多個專案，並對每個專案平行派發任務，任務在隔離的 worktree 中執行到完成、通過驗收。

### B-1 Project registry（M） #11
- [ ] `fleet.Project`：ID、名稱、repo 路徑、remote、預設分支、信任模式、驗收指令、並行上限、分支前綴
- [ ] SQLite 表 `fleet_projects`，CRUD 與測試
- [ ] 登記時檢查：路徑是 git repo、預設分支存在
- [ ] 專案可附加任意 key-value 綁定（供 TG topic 等使用）
- [ ] 依賴：A-1

### B-2 Task 模型與狀態機（M） #12
- [ ] `fleet.Task`：ID、專案、shape、目標文字、來源（手動或 issue 編號）、狀態、分支、worktree 路徑、session ID、token 與成本、時間戳
- [ ] 合法轉移表與 `Transition()`，非法轉移回傳錯誤
- [ ] SQLite 表 `fleet_tasks`、`fleet_task_events`（只增不改）
- [ ] 每個轉移都有表格式測試
- [ ] 依賴：B-1

### B-3 `worktree/` 套件（M） #13
- [ ] `Create(repo, branch, base)`、`Remove(path)`、`List(repo)`、`IsDirty(path)`
- [ ] `Remove` 遇到髒的 worktree 回傳 `ErrDirty`，需明確傳入 force
- [ ] worktree 集中放在可設定的根目錄下，路徑含專案與任務 ID
- [ ] 以暫時 repo 做整合測試
- [ ] 依賴：A-1

### B-4 Runner：一個任務一個 agent 程序（L） #14
- [ ] 在任務的 worktree 內啟動 `claude`，串流事件寫入 `fleet_task_events`
- [ ] 以 `--resume` 支援中斷後接續與追加指示
- [ ] 取消：先送中斷，逾時後強制結束
- [ ] 記錄 token、成本、最後活動時間
- [ ] scout 任務以工具白名單限制為唯讀，並要求輸出 `report.md`
- [ ] 以假的 `claude` 執行檔測試完整生命週期
- [ ] 依賴：B-2、B-3

### B-5 Scheduler 與並行控制（M） #15
- [ ] 全域與每專案的並行上限
- [ ] 佇列依優先序與建立時間出隊
- [ ] 同一分支不可同時有兩個執行中的任務
- [ ] 依賴：B-4

### B-6 Watcher：零 token 監督（L） #16
- [ ] 單一 goroutine 監看所有執行中的任務
- [ ] 以程序狀態、最後事件時間、輸出內容推導 `busy / idle / unknown / dead`
- [ ] 分類需要處理的事件：完成、失敗、卡住（超過門檻無活動）、agent 提問、超出預算
- [ ] 可處理事件寫入持久的 `fleet_wakes` 佇列，消費後才標記
- [ ] 訊號缺失時判為 `unknown` 的測試
- [ ] 依賴：B-4

### B-7 驗收閘門（M） #17
- [ ] 任務進入 `verifying` 時依序執行專案的驗收指令
- [ ] 結果（指令、結束碼、輸出尾段、耗時）存入 `fleet_verifications`
- [ ] 失敗時把輸出回饋給同一個 session 重試，次數可設定，用完轉 `needs_decision`
- [ ] scout 任務的驗收為：`report.md` 存在且 worktree 無變更
- [ ] 依賴：A-5、B-4

### B-8 信任模式與交付（L） #18
- [ ] `local-only`：驗收通過後停在 `review_ready`，不 push
- [ ] `direct-pr`：push 分支並開 PR，記錄 PR 連結
- [ ] `no-mistakes`：開 PR 後等待核准，核准後才合併。核准者只有專案擁有者一人，流程為單一核准動作，不做多人審核
- [ ] 合併只走 fast-forward 或 PR 合併，不改寫歷史
- [ ] 模式在派工時固定，執行中不可變更
- [ ] 依賴：B-7

### B-9 重啟復原（M） #19
- [ ] 啟動時掃描非終態任務，與實際程序、worktree 對帳
- [ ] 程序已不存在的 `running` 任務標為 `dead`，轉 `needs_decision`
- [ ] 孤兒 worktree 列出報告，不自動刪除
- [ ] 依賴：B-6

### B-10 預算與配額（S） #20
- [ ] 每任務、每專案、每日的 token 與成本上限
- [ ] 超出時暫停任務並產生 wake 事件
- [ ] 依賴：B-6

### B-11 GitHub issue 作為任務來源（M） #21
- [ ] 以 issue 編號派工，issue 內容與未勾選的 checklist 帶入目標
- [ ] 任務開始、完成、失敗時在 issue 留言
- [ ] 驗收通過後同步 checklist，並附上驗收證據
- [ ] 依賴：A-3、B-7

---

## Epic C：Command API（alice-core） #3

目標：TG 與網頁共用的單一指揮介面。兩個前端都不直接碰 `fleet` 的內部。

### C-1 `fleet.Commander` 介面（M） #22
- [ ] 指令：`RegisterProject`、`UpdateProject`、`Dispatch`、`Cancel`、`Retry`、`Reply`（回答 agent 提問或追加指示）、`Approve`、`Merge`、`Discard`
- [ ] 查詢：`ListProjects`、`ListTasks`（可依專案、狀態篩選）、`GetTask`、`TaskEvents`（分頁）、`TaskDiff`、`Digest`
- [ ] 所有指令帶上操作者身分並寫入稽核紀錄
- [ ] 依賴：B-2

### C-2 事件匯流排（M） #23
- [ ] `Subscribe(filter)` 回傳事件 channel：任務狀態變更、存活狀態變更、驗收結果、wake
- [ ] 支援從指定事件 ID 之後重播，供斷線重連
- [ ] 與既有的 `agent.EventSink` 相容
- [ ] 依賴：B-6

### C-3 通知政策（M） #24
- [ ] 每位使用者的模式：`normal`、`quiet`（只收需要決策的）、`afk`（在授權範圍內自動處理，回來時給摘要）
- [ ] `afk` 的授權範圍明確列舉且有到期時間，到期自動回到 `normal`
- [ ] 同一任務的通知合併，設最短間隔
- [ ] 依賴：C-2

### C-4 Digest 產生器（S） #25
- [ ] 艦隊摘要：各專案執行中、待審、待決策、失敗的數量，以及今日成本
- [ ] 輸出結構化資料，由前端各自排版
- [ ] 依賴：C-1

---

## alice 現況（撰寫 Epic D、E、F 時對照的事實）

以下根據 `chimerakang/alice` 的 `exp/hermes-vscode-hook` 分支（提交 `24d0b11`）。

- **開發線不在 `main`**：`exp/hermes-vscode-hook` 領先 `main` 234 個提交，含 `engine/`、`issueops/`、`process/`、`task/`；`main` 只多出 3 個 Telegram 可靠性與 wake 的修正。`alice-core` 是從這個分支抽出來的。
- **alice 尚未引用 `alice-core`**：兩邊各有一份 hermes／engine 程式碼。
- **Telegram**：`telegram.go` 約 11,000 行。topic 對專案的綁定已存在（`/project` 加 `topic_settings` 表）。一般文字訊息是與 agent 直接對話，任務由 `/hermes`、`/ghermes` 啟動。inline 按鈕以前綴分流（`hermes:`、`retry:`、`menu:`、`tasks:`）。使用者可見文字一律走 `locales/` 的 i18n。
- **Web**：REST 與 `/ws` 都掛在 `WebInterface`（port 8082），控制端點以 Bearer `apiToken` 保護。型別採 proto-first（`proto/` 產生 `gen/`）。前端為 React 19、Vite、Tailwind 4、zustand、recharts，已有 `DiffViewer`、`TimelineEntry`、`ToolCallGantt`、`StatusBadge` 元件，以及 `/issues-runs`、`/run-inspector`、`/hermes-tasks`、`/reviews`、`/quality` 等頁面。
- **會被取代的既有 issue**：alice 的 #177、#173、#172、#158、#115 都以 hermes 為前提，切換到 fleet 後需關閉或改寫。

---

## Epic D：Telegram 指揮介面（alice） alice#179

目標：在手機上完成派工、掌握狀況、做決策。不在 TG 內呈現長篇 log 或完整 diff，改以連結導向網頁。所有新文字走 `locales/` 的 i18n，新的 callback 一律使用 `fleet:` 前綴。

### D-1 既有 topic 綁定接上 fleet 專案登記（M）
topic 對專案路徑的綁定已由 `/project` 與 `topic_settings` 表提供，這裡只做對接，不重做綁定。
- [ ] `/project <路徑>` 成功時，若該路徑尚未登記為 fleet 專案則自動登記
- [ ] 啟動時把 `topic_settings` 既有的路徑回填為 fleet 專案，重複執行不產生重複資料
- [ ] `/project` 不帶參數時顯示目前專案的信任模式、驗收指令、並行上限
- [ ] `/project mode <no-mistakes|direct-pr|local-only>` 設定信任模式
- [ ] 未綁定專案的 topic 收到派工指令時提示先執行 `/project`
- [ ] 依賴：B-1、C-1、F-5

### D-2 派工指令（M）
一般文字訊息維持與 agent 直接對話，不自動派工。
- [ ] `/ship <目標>` 派發 ship 任務到本 topic 的專案
- [ ] `/scout <問題>` 派發調查任務
- [ ] `/ship #<編號>` 以 GitHub issue 派工，沿用既有的 issue 品質檢查
- [ ] 可用 `--model`、`--budget` 覆寫單次任務的模型與預算
- [ ] 回覆的確認訊息就是該任務的卡片（見 D-3）
- [ ] 指令解析有表格式測試
- [ ] 依賴：D-1、B-11

### D-3 任務卡片（M）
- [ ] 每個任務一則訊息，狀態變更時原地編輯，不洗版
- [ ] 顯示：狀態、已執行時間、目前動作的一行摘要、成本
- [ ] 按鈕依狀態變化：取消、重試、看詳情（開網頁）、核准、合併、捨棄
- [ ] callback data 使用短任務 ID，長度不超過 Telegram 的 64 bytes 上限
- [ ] 編輯訊息的頻率有下限間隔，並走既有的 Telegram 可靠性層
- [ ] bot 重啟後仍能更新重啟前送出的卡片（訊息 ID 需持久化）
- [ ] 依賴：C-2、F-5

### D-4 決策提示（M）
沿用既有 `hermes:fail:`、`hermes:budget:` 提示的互動樣式。
- [ ] agent 提問、驗收用盡重試、超出預算、任務卡住、程序死亡時發出提示
- [ ] 常見選項以按鈕呈現：重試、帶提示重試、放行、捨棄
- [ ] 回覆該提示訊息即把文字送入對應任務的 session
- [ ] 同一任務同一原因不重複提示
- [ ] 依賴：D-3

### D-5 艦隊總覽指令（S）
- [ ] `/fleet`：跨專案摘要，列出各專案執行中、待審、待決策、失敗的數量與今日成本
- [ ] `/fleet tasks`：本 topic 專案的任務清單，可點選開啟卡片
- [ ] `/status` 與 `/menu` 加入 fleet 的狀態與入口
- [ ] 既有的 `/tasks`（讀取 GitHub issues）維持不變
- [ ] 依賴：C-4

### D-6 通知模式（S）
- [ ] `/quiet`：只收需要決策的通知
- [ ] `/afk <時長> [授權範圍]`：在授權範圍內自動處理，到期自動恢復
- [ ] `/back`：恢復一般模式，並回傳離開期間的摘要
- [ ] 模式在 bot 重啟後保留
- [ ] 依賴：C-3

### D-7 交付結果訊息（S）
- [ ] 完成時送出：變更檔案數、增刪行數、驗收結果、PR 連結
- [ ] `no-mistakes` 模式附上單一「核准並合併」按鈕，僅擁有者可按
- [ ] scout 任務送出報告摘要，並以檔案附上 `report.md`
- [ ] 依賴：D-3、B-8

### D-8 既有指令的相容對應（M）
切換後舊指令不可失效。
- [ ] `/hermes`、`/ghermes` 在 fleet 模式下等同 `/ship`，並提示新指令名稱
- [ ] `/abort`、`/retry`、`/resume`、`/close` 對應到 fleet 的取消、重試、接續、捨棄
- [ ] `/parallel` 改為一次派發多個 fleet 任務
- [ ] `/strict` 對應到 `no-mistakes` 信任模式
- [ ] `/mr` 在 fleet 模式下的行為另行決定，本項先標示為不支援並回覆說明
- [ ] 依賴：D-2、D-3

---

## Epic E：Web 指揮中心（alice） alice#180

目標：完整的艦隊管控介面，涵蓋 TG 放不下的所有內容。前端變更後需依既有流程重建並複製到 `web/`。

### E-1 Fleet 的 HTTP 與 WebSocket 轉接層（M）
- [ ] 在 `proto/` 定義 fleet 的專案、任務、事件、驗收結果型別，產生 `gen/` 與前端型別
- [ ] 在既有 `WebInterface` 下新增 `/api/fleet/*`：專案、任務、事件、diff、摘要的查詢端點
- [ ] 指令端點（派工、取消、重試、回覆、核准、合併、捨棄）沿用既有的 Bearer `apiToken` 檢查
- [ ] 既有 `/ws` hub 新增 `fleet.*` 訊息型別，支援以事件 ID 續傳
- [ ] 前端 `lib/api.ts` 與 zustand store 加上 fleet 的 client 與狀態
- [ ] 每個端點有 handler 測試
- [ ] 依賴：C-1、C-2、F-5

### E-2 艦隊看板（L）
- [ ] 新增 `/fleet` 頁並放在導覽第一位，`/issues-runs` 保留
- [ ] 以狀態分欄的看板，橫跨所有專案，可依專案篩選
- [ ] 卡片即時更新，存活狀態以顏色區分，`unknown` 有獨立樣式
- [ ] 頂部顯示全域計數與今日成本
- [ ] 手機寬度下可用
- [ ] 依賴：E-1

### E-3 任務詳情頁（L）
- [ ] 即時串流的事件時間軸，重用 `TimelineEntry` 與 `ToolCallGantt`
- [ ] Diff 檢視重用 `DiffViewer`，資料來源為任務 worktree 對基準分支的差異
- [ ] 驗收結果：每個指令的輸出與結束碼
- [ ] 操作列：取消、重試、追加指示、核准、合併、捨棄
- [ ] 可由 TG 卡片的「看詳情」連結直接開啟
- [ ] 依賴：E-1

### E-4 派工編輯器（M）
- [ ] 選擇專案、shape、模型、預算上限
- [ ] 批次派工：一次輸入多個任務
- [ ] 從 GitHub issue 清單勾選派工
- [ ] 依賴：E-1、B-11

### E-5 決策收件匣（M）
- [ ] 集中列出所有 `needs_decision` 與 `review_ready` 的任務
- [ ] 每一項附上情境與可選動作，不必進入詳情頁即可處理
- [ ] 導覽列顯示待處理數量
- [ ] 依賴：E-1

### E-6 專案管理頁（M）
- [ ] 登記與編輯專案：路徑、信任模式、驗收指令、並行上限
- [ ] 驗收指令可試跑並顯示結果
- [ ] 顯示該專案的 worktree 清單，標示髒的與孤兒的
- [ ] 顯示該專案綁定的 TG topic
- [ ] 依賴：E-1

### E-7 成本與用量擴充（M）
擴充既有的 Performance 頁與 `/api/costs/*`，不另建新頁。
- [ ] 成本可依專案與任務分組
- [ ] 顯示預算使用率與超限紀錄
- [ ] 依賴：E-1、B-10

### E-8 稽核紀錄擴充（S）
擴充既有的 Security 頁稽核區塊。
- [ ] 記錄 fleet 指令的操作者、時間、來源（TG 或網頁）、結果
- [ ] 可依專案與任務篩選
- [ ] 依賴：E-1

---

## Epic F：切換與移除 hermes #4、alice#181

alice 端的項目為 F-1、F-4、F-5、F-6；alice-core 端為 F-2、F-3。

### F-4 統一開發基準分支（alice，S）
`exp/hermes-vscode-hook` 與 `main` 已分岔，fleet 的工作需要單一基準。
- [ ] 決定基準分支（建議以 `exp/hermes-vscode-hook` 為準）
- [ ] 把另一條線獨有的提交併入：`main` 上的 Telegram 請求逾時、輪詢復原、`/wake` 指令
- [ ] 併入後 `go build ./...` 與 `go test ./...` 通過
- [ ] 預設分支指向統一後的結果

### F-5 引用 alice-core 並接上 fleet（alice，M）
- [ ] `go.mod` 加入 `github.com/chimerakang/alice-core`
- [ ] 啟動時建立 fleet 實例，與既有 SQLite 資料庫及單一實例鎖共用
- [ ] fleet 的事件轉送到既有的 WebSocket hub 與 runtime events
- [ ] 設定檔範本加入 fleet 區塊：worktree 根目錄、全域並行上限、預設信任模式
- [ ] fleet 未啟用時行為與現在完全相同
- [ ] 依賴：F-4、B-4、C-1

### F-1 Alice 改走 fleet（alice，L）
- [ ] 以設定開關在 hermes 與 fleet 之間切換，預設仍為 hermes
- [ ] 在至少兩個真實專案上以 fleet 完成十個任務並記錄結果
- [ ] 預設值切到 fleet
- [ ] 關閉或改寫以 hermes 為前提的既有 issue：#177、#173、#172、#158、#115
- [ ] 依賴：F-5、D-2、D-3、D-4、D-7、D-8

### F-6 移除 alice 內的舊協調程式碼與頁面（alice，M）
- [ ] 刪除 `internal/app/hermes/`、`internal/app/engine/`、`internal/app/task/` 及其 bridge 檔案
- [ ] 刪除 `multiagent.go` 的 `AgentCoordinator` 與 `SubAgentOrchestrator`
- [ ] `internal/app/issueops/`、`internal/app/process/` 改用 alice-core 的版本
- [ ] 移除 `/hermes-tasks`、`/reviews`、`/quality` 頁面與 `/api/hermes/*`、`/api/quality/*` 端點，或改為唯讀的歷史檢視
- [ ] 更新 README、CLAUDE.md 與 `docs/`
- [ ] 依賴：F-1

### F-2 舊資料處理（alice-core，S） #26
- [ ] 舊的 `hermes_*` 表保留唯讀，不做遷移
- [ ] 提供匯出歷史任務摘要的指令
- [ ] 依賴：F-1

### F-3 移除 hermes 與 engine 的 planner 程式碼（alice-core，M） #27
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
| 3 | F-4、F-5、D-1 至 D-4、D-7、B-8、B-9 | alice 接上 alice-core，TG 上可派工、看狀態、做決策、交付 |
| 4 | E-1 至 E-3、E-5 | 網頁看板、任務詳情、決策收件匣 |
| 5 | 其餘 B、C、D、E 項目 | 預算、通知模式、成本、稽核 |
| 6 | D-8、F-1、F-6、F-2、F-3 | 切換到 fleet，移除兩邊的 hermes |

階段 1 與階段 2 的 B-1 至 B-3 沒有相依，可以同時進行。

## 待確認事項

1. alice 的開發基準分支（見 F-4）。本文件的 Epic D、E 以 `exp/hermes-vscode-hook` 為對照。
2. worktree 根目錄與磁碟配額的預設值。

## 已確認事項

- `no-mistakes` 模式的核准者只有專案擁有者一人，核准流程為單一動作。
- Epic D、E 與 alice 端的 F 項目建立在 `chimerakang/alice`，里程碑為 `P17 - Fleet Command`。
