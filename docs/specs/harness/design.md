# NAVETTE — Design

- Status: Draft (awaiting review)
- Date: 2026-10-14
- Spec location: `navette/docs/specs/harness/`
- Inputs: `requirements.md` (Approved w/ conditions), ADR-0001〜0004 (all Accepted)
- Module path: `github.com/habuka036/navette`

## Requirement mapping

| Requirement | Design section |
|---|---|
| R1 タスク定義・状態機械 | Domain model / State machine |
| R2 永続化(SQLite+Markdown) | Persistence (RD-3: SQLite=正, Markdown=参照) |
| R3 AgentRuntime 差替(Kiro CLI) | Ports: AgentRuntime / kiroruntime |
| R4 LLMProvider 差替(Ollama) | Ports: LLMProvider / ollamaprovider |
| R5 実行境界(timeout/cancel/反復) | Control loop / Execution bounds (RD-4) |
| R6 承認ゲート(事前/結果) | Approval model / Control loop |
| R7 隔離・Git 制限 | Execution isolation (RD-5) |
| R8 Une Feuille 任意性 | Memory integration (optional) |

## 調査済み前提: Kiro CLI（実機確認, v2.21.1）

本設計はこの環境の `kiro-cli` を実際に調査した結果に基づく（RD-1）。確認済みの事実：

- `kiro-cli chat [INPUT] --no-interactive`：ユーザー入力を期待せず起動する headless 実行。
- `--output-format stream-json`：実行の ACP イベントを **JSON Lines** で stdout に出力する。
  `--no-interactive` を含意する。**ただし v1 エンジンでは不可で `--agent-engine v2`（または
  v3）が必須**（NAV-TASK-001 で実機確認）。NAVETTE はこれを機械可読チャネルとして解析する。
- `--trust-tools <names>` / `--trust-all-tools` / `--trust-tools=`（空＝何も信頼しない）：
  確認なしに実行してよいツールを指定する起動オプション。**ただし NAV-TASK-001 の実機実験で、
  非対話モードでは `--trust-tools=`（空）でもツールが実行された**。したがって `--trust-tools`
  は NAVETTE の承認ゲートの基盤に**しない**。安全設計の基本は、NAVETTE 側の**事前承認**と
  **bubblewrap による実行環境の隔離**である（§Approval model, §Execution isolation）。
- `--agent` / `--model` / `--effort` / `--resume-id`：セッション・設定の指定。
- **エンジン指定の区別（実機確認）**：
  - トップレベルの `--v3` は「次世代 Kiro agent を起動する」ための起動フラグ（UI/セッション
    ランチャ寄り。既定 UI の代替）。現在の対話セッションは `kiro-cli --v3` 起動。
  - `kiro-cli chat --agent-engine <v1|v2|v3>`（既定 v2）は chat サブコマンドの**エンジン選択**。
    `stream-json` は v2 または v3 で可。
  - 両者は別の指定。NAVETTE の Kiro Runtime は **Agent Engine を設定可能**にし、**v2 を
    ハードコードしない**。v2/v3 の stream-json 互換性・承認・終了動作の比較は Milestone C の
    実機接続時に行う（本 Milestone B では実 CLI を起動しない）。

未確認事項は「推測で実装しない」（RD-1）。具体的な stream-json スキーマ（イベント型・
フィールド）は実装着手時に実イベントを採取して確定する（下記 §保証範囲と未確認を参照）。

## Current architecture

NAVETTE は新規リポジトリ。既存コードなし。本設計が初期構成を定義する。

## Proposed design

### パッケージ構成（案）

```
navette/
  go.mod                              ← module github.com/habuka036/navette
  cmd/navette/main.go                 ← CLI エントリ（タスク登録・実行・承認）
  internal/
    task/                             ← ドメイン型・状態機械（Control Plane 中核）
      task.go  state.go  transition.go
    store/                            ← 永続化（SQLite 正 + Markdown 参照）
      sqlite.go  migrations/  markdown.go
    engine/                           ← 制御ループ（timeout/cancel/反復/承認仲介）
      engine.go  bounds.go
    runtime/                          ← AgentRuntime ポートと実装
      runtime.go                      ← interface
      kiroruntime/kiro.go             ← Kiro CLI 実装（stream-json 解析）
    provider/                         ← LLMProvider ポートと実装
      provider.go                     ← interface
      ollamaprovider/ollama.go        ← Ollama 実装
    approval/                         ← 承認ゲート（CLI 対話）
      approval.go
    memory/                           ← Une Feuille 連携（optional）
      memory.go                       ← unefeuille.Store を optional に束ねる
    exec/                             ← コマンド実行ラッパ（allowlist / Git 制限）
      guard.go
```

Control Plane（task + engine + store + approval）は NAVETTE が保持し、runtime / provider /
memory は差し替え可能なポートとして分離する（ADR-0001/0002, NFR-004）。

### Domain model（R1）

```go
type Task struct {
    ID         string
    Goal       string
    Input      map[string]any
    Constraints Constraints   // timeout, maxIterations, allowed workdir 等
    State      State
    CreatedAt  time.Time
    UpdatedAt  time.Time
}

type Constraints struct {
    Timeout       time.Duration
    MaxIterations int           // Control Plane ループ回数の上限 (RD-4)
    WorkDir       string        // Agent が書込可能な一時作業コピーのパス（.git 除外）。
                                // 実リポジトリそのものではない（§Execution isolation）。
}

type Run struct {        // 1 タスクの 1 回の実行試行
    ID        string
    TaskID    string
    StartedAt time.Time
    EndedAt   time.Time
    Outcome   Outcome    // Succeeded/Failed/Cancelled/TimedOut/Rejected
}

type Event struct {      // 状態遷移・実行イベント（SQLite が正, RD-3）
    ID        string
    TaskID    string
    RunID     string
    At        time.Time
    Kind      string     // "state.transition" | "approval" | "runtime.output" | ...
    From, To  State      // state.transition のとき
    Reason    string
    Detail    json.RawMessage
}
```

### State machine（R1, R5, R6）

状態名は必要最小限とする（複雑なワークフローエンジンにはしない）。

```
Pending
  → AwaitingPreApproval       (Runtime 起動前の承認待ち)
      → Running               (事前承認済み。ここで初めて Runtime を起動)
      → Rejected (terminal)   (事前承認が却下された)
  Running
      → AwaitingResultApproval (Runtime 終了。結果の受理待ち)
          → Succeeded (terminal)  (結果承認)
          → Rejected  (terminal)  (結果却下)
      → Failed    (terminal)   (実行エラー)
      → Cancelled (terminal)   (キャンセル)
      → TimedOut  (terminal)   (タイムアウト)
Indeterminate                  (再起動時の不確定; R2 AC-4)
```

- 事前承認と結果承認は**別の状態・別のイベント**として扱う（R6 AC-1）。
  - `AwaitingPreApproval`：**事前承認なしに Runtime を起動しない**（R6 AC-2/3）。
  - `AwaitingResultApproval`：Runtime 終了後の結果承認。事前承認とは独立に記録する。
  - 結果承認後の終端状態は `Succeeded`（承認）または `Rejected`（却下）で明確化する。
- 遷移は `transition.go` の許可表で定義。許可外の遷移要求は拒否し状態を変えない（R1 AC-4）。
- **状態変更とイベント記録は 1 つの SQLite トランザクション**で行い、整合性を保つ
  （状態と履歴が食い違わない）。
- 再起動時に Running 中断は **Indeterminate** とし、成功/失敗へ暗黙遷移しない（R2 AC-4）。
  未承認（AwaitingPreApproval / AwaitingResultApproval）は当該状態を維持し、未承認操作を
  **自動再開しない**（R6 AC-4）。再開には改めて承認を要する。

### Approval model（R6: 事前承認と結果承認の区別）

2 種類の承認を独立に扱う（R6 AC-1）。状態機械上も別状態
（`AwaitingPreApproval` / `AwaitingResultApproval`）として分離する。

- **PreExecApproval（事前承認）**：Runtime/重要操作を**起動してよいか**。事前承認なしに
  Runtime を起動しない（R6 AC-2/3）。却下で `Rejected`（terminal）。
- **ResultApproval（結果承認）**：Runtime 終了後の結果を**受理・確定してよいか**。事前承認
  とは独立に記録する。承認で `Succeeded`、却下で `Rejected`（いずれも terminal）。
  **結果承認は「成果物（一時作業コピーの差分・ログ）の受理」を意味し、実リポジトリへの自動
  反映や Git commit を意味しない**（§Execution isolation）。

```go
type ApprovalKind int // PreExec | Result
type Decision struct {
    Kind     ApprovalKind
    Approved bool
    By       string
    At       time.Time
    Reason   string
}
```

- 承認は `approval.Gate` が CLI 対話（標準入力 y/n）で取得する。
- 承認レコード・承認イベント・状態遷移・遷移イベントは **`store.ApproveAndTransition` が
  単一 SQLite トランザクション**で確定する。トランザクション内で現在状態と承認種別を検証し、
  不正な状態（`ErrUnexpectedState`）・重複承認（`ErrDuplicateApproval`）を拒否して部分記録を
  残さない（R6 AC-5, 実装済み）。
- CLI 切断・プロセス再起動時：未承認状態（AwaitingPreApproval / AwaitingResultApproval）を
  維持。再起動後に未承認操作を**自動再開しない**（R6 AC-4）。再開には改めて承認を要する。
- 非対話実行中に承認が必要になったら、無断実行せず AwaitingPreApproval（または結果承認が
  必要なら AwaitingResultApproval）へ安全移行して停止（R6 AC-6）。

### Control loop（R5, RD-4）

```go
// RunTask returns an Outcome{State, Err, PersistError}; it does not panic on
// cancellation/timeout and surfaces persistence failures explicitly.
func (e *Engine) RunTask(ctx context.Context, taskID string) Outcome
```

- `ctx` に Task.Timeout の `context.WithTimeout` を被せる。超過で TimedOut（R5 AC-1, NFR-001）。
- 外部キャンセルは同じ `ctx` の cancel で伝播し Cancelled（R5 AC-2）。
- ループ回数 `iteration` を数え、`MaxIterations` 超過で打ち切り（RD-4: **Control Plane の
  ループ回数**で定義。Agent 内部反復は Runtime が制御可能な場合のみ別途）。
- 打ち切り時は理由を Event に記録し、部分成果物を保持（R5 AC-4）。
- 各反復：Runtime 起動は**事前承認後のみ**（AwaitingPreApproval を経由）。クラウド
  Provider（`IsCloud()==true`）呼び出しも事前承認を要する。初期 PoC の承認はタスク単位
  （Runtime 起動単位）であり、Kiro CLI 内部の操作単位承認は保証しない（RD-2, §kiroruntime）。

### Ports（差し替え可能インターフェース, R3/R4, NFR-004）

**AgentRuntime と LLMProvider の関係（重要）：**

- **Kiro CLI 自体が LLM を利用する Agent Runtime** である。Kiro CLI は内部で自前のモデルを
  用いてタスクを遂行する。
- NAVETTE の `LLMProvider` は、**Kiro CLI 内部のモデルを自動的に置き換えるものではない**。
  `LLMProvider` は、NAVETTE 自身や Provider を必要とする Runtime が LLM 呼び出しを行うための
  抽象であり、Kiro Runtime の内部モデル選択とは独立。
- Milestone B では **fake AgentRuntime だけでタスクを完結**できる構成にする。`LLMProvider` が
  未設定でも、Provider を必要としないタスクは実行可能（R8 と同様に optional 扱い）。

```go
// runtime.AgentRuntime: Agent 実行方式の抽象
type AgentRuntime interface {
    // Execute は 1 ステップの Agent 実行を行い、構造化イベント列を返す。
    Execute(ctx context.Context, req RunRequest) (RunResult, error)
    Name() string
}

// provider.LLMProvider: LLM 呼び出しの抽象（optional）
type LLMProvider interface {
    Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error)
    Name() string
    // IsCloud は true のときクラウド API 呼び出しであることを示し、
    // Engine は呼び出し前に PreExecApproval を要求する (R4 AC-3)。
    IsCloud() bool
}
```

- Runtime 未設定時：タスク実行開始で明示エラー（R3 AC-4）。暗黙 no-op しない。
- Provider 未設定時：Provider を必要としないタスクはそのまま実行可能（未設定自体はエラーに
  しない）。Provider を要する処理に到達して初めて、未設定なら明示エラーとする。

### kiroruntime（R3, 調査事実に基づく）

- 起動：`kiro-cli chat <prompt> --no-interactive --output-format stream-json
  --agent-engine v2 [--agent ...] [--model ...] [--trust-tools=...]`。
  **実機確認（NAV-TASK-001）**：`--output-format stream-json` は v1 エンジンで不可。
  `--agent-engine v2`（または v3）が必須。
- stdout の JSON Lines を逐次パースし、`RunResult` の構造化イベントへ変換。実機で確認した
  `type`：`runStarted` / `metadata` / `sessionUpdate`(agent_message_chunk / tool_call /
  tool_call_update) / `runFinished`(status/stopReason/finalText) / `runError`(stage/message)。
  （詳細は `notes/milestone-a-experiments.md`。）
- **成否判定は exit code に依存しない**：無効 `--model` でも警告のみで exit 0 になる実機
  挙動を確認済み。`runFinished.status` と `runError` の有無で判定する。
- **起動オプションによる最小化**：`--trust-tools=`（空）または必要最小の allowlist を渡し、
  `--trust-all-tools` は使わない。
- **承認制御の範囲（重要・実機確認）**：NAV-TASK-001 で、`--no-interactive --trust-tools=`
  （信頼ツールなし）でもエージェントが write ツールを実行しファイルを作成した。すなわち
  **`--trust-tools` は非対話モードでツール実行をブロックせず、NAVETTE の承認ゲートの根拠に
  できない**。初期 PoC で NAVETTE が保証するのは**実行前のタスク単位の承認**（Runtime を
  起動してよいか）と**実行環境の隔離**である。Kiro CLI 内部の操作単位承認は**未確認・
  未実装**とし、実機検証で対応可能と判明した場合にのみ追加する（RD-2）。
- タイムアウト/キャンセル：親 `kiro-cli` プロセスへ ctx 経由で伝播し終了させる。ただし
  **実機確認（NAV-TASK-001）で、信頼ツールが起動した子孫プロセス（例 `sleep 300 &`）は
  親終了後も残りうる**ことを確認。プロセスツリー kill だけでは不十分なため、プロセスグループ
  単位 kill ＋隔離環境ごとの破棄で回収する（実装時に方式確定）。

### ollamaprovider（R4）

- 既定ローカルエンドポイント（例 `http://localhost:11434`）へ HTTP。`IsCloud()==false`。
- タイムアウト/エラー時は Engine がタスクを Failed/Cancelled へ安全遷移（R4 AC-4）。
- OpenAI 等クラウド Provider は将来追加。`IsCloud()==true` を返し承認必須（SEC-001）。

### Persistence（R2, RD-3）

- **SQLite が状態・イベント履歴の正**（source of truth）。pure-Go ドライバ
  （`modernc.org/sqlite`、MILLEFEUILLE と同じ系統）を用い CGO 不要（NFR-002）。
  テーブル：`tasks` / `runs` / `events` / `approvals`。WAL + マイグレーション。
- **Markdown は人間向けの参照・レビュー用成果物**（RD-3）。SQLite から派生生成する
  読み取りビュー（タスクサマリ・実行ログ・承認履歴）。正ではない。
- 再起動時は SQLite から状態・履歴を復元（R2 AC-3）。Running 中断は Indeterminate（R2 AC-4）。

### Execution isolation & Git restriction（R7, RD-5）

steering だけに依存せず多層で担保する（ADR-0003, RD-5）。初期 PoC は「許可対象の作業ツリー
だけを書き込み可能にする隔離実行環境」を最優先とする。

**制御できる境界と限界：**

- `exec.Guard` が制御できるのは、**NAVETTE が直接起動するコマンドの境界**（引数・作業
  ディレクトリ・環境変数）に限る。NAVETTE が起動した Kiro CLI プロセスが内部で生成する
  **子プロセスまで `exec.Guard` が自動的に制御できるとは保証しない**。したがって Git 操作の
  抑止を allowlist だけに依存せず、下記の隔離を主たる担保とする。

**PoC で優先する隔離実行環境（主たる担保）：**

- **書き込み可能範囲の限定**：許可対象の作業ツリー（`Constraints.WorkDir` 配下）**のみ**を
  書き込み可能にする。対象外リポジトリへの書き込みを禁止する。
- **Imagorium の 3 リポジトリは原則読み取り専用**（参照のみ。書き込みを許さない）。
- **`.git` メタデータの保護**：対象作業ツリーを含め `.git/` への書き込み・Git 状態変更操作
  （commit/push/merge/reset/checkout 等）を禁止する。
- **秘密情報・ホスト資源を不用意に公開しない**：認証情報、ホストの SSH Agent、Docker
  Socket 等を実行環境へ露出しない。
- **隔離が未完成なら自律書き込みを許可しない**：隔離実行環境が整うまで、実リポジトリに
  対する Agent の自律的な書き込み実行を許可しない（安全側に倒す）。

**実現方式の比較（DevContainer 内でさらに隔離する場合）：**

Docker Socket の公開や特権コンテナを安易に前提にしない。候補を比較して選定する。
**実機確認（NAV-TASK-002）：bubblewrap(bwrap) が非特権で利用可能**であり、作業ツリーのみ
書込・repo/.git の read-only・ネットワーク遮断・home 非露出を検証済み。これを主たる隔離手段
とする。

| 方式 | 書き込み隔離 | 秘密/ホスト資源露出 | PoC 実現容易性 | 備考 |
|---|---|---|---|---|
| **bubblewrap（非特権）** | **高（bind/ro-bind）** | **低（home/net 遮断）** | **高（実機確認済）** | **採用候補** |
| 作業ツリーの一時 copy + 書込制限 dir | 中（copy 先のみ可） | 低 | 高 | 補助 |
| OS 権限 / 別ユーザ + ディレクトリ ACL | 中〜高 | 低 | 中 | 補助 |
| ネスト DevContainer / DinD（Docker Socket） | 高 | **高（socket 露出）** | 低 | 安易に前提にしない |
| 特権コンテナ | 高 | **高** | 低 | 原則避ける |

→ **PoC の最小隔離構成（.git 除外の一時作業コピー方式）**：

- **実リポジトリを直接書き換えない。** Agent には **`.git` を除外した一時作業コピー**を渡し、
  Agent はその一時コピーのみを変更する。実リポジトリの `.git` を Agent に書込可能な形で
  公開しない（`WorkDir` 全体を書込可にして `.git` まで書込可になる設計を避ける）。
  - 対象リポジトリの作業ツリーを一時ディレクトリへコピー（`.git` 除外）。
  - bubblewrap で一時作業コピーのみを `--bind`（書込可）。
  - Imagorium 3 リポジトリ等の参照対象は `--ro-bind`（読み取り専用）。
  - `--unshare-net` で既定ネットワーク遮断。home / SSH Agent / Docker Socket / 認証情報は
    bind しない。
- **実行結果の反映は人間が行う**：Agent が一時作業コピーに加えた差分を人間がレビューする。
  **結果承認は「成果物の受理」を意味し、実リポジトリへの自動反映や Git commit を意味しない**。
  実リポジトリへの変更反映（および commit）は初期 PoC では人間が明示的に実施する。
- 具体化・実効性テストは Milestone C（NAV-TASK-022）で行う。実験は秘密情報を含まない一時
  ディレクトリまたは使い捨て環境で実施する。

- NAVETTE 自身も commit 等を行わない（ADR-0003）。確定は人間/コンテナ外。

### Memory integration（R8, optional）

- `memory.Recorder` が `unefeuille.Store`（`github.com/habuka036/millefeuille/unefeuille`）を
  **optional** に保持。nil なら記憶連携をスキップ（R8 AC-2）。
- PoC では Une Feuille を必須依存にしない。NAVETTE 単独で SQLite+Markdown により動作
  （R8 AC-1, COMP-001）。
- Store 設定時は Run ログ・成果物要約を `Save` で記録（R8 AC-3）。有無で状態機械の基本挙動を
  変えない（R8 AC-4）。

## Data flow

```
cmd/navette ──> engine.RunTask(ctx,timeout)
                   │  （反復ループ, MaxIterations, 承認仲介）
                   ├─ provider.LLMProvider.Complete  (Ollama; IsCloud→承認)
                   ├─ runtime.AgentRuntime.Execute   (kiro-cli --no-interactive
                   │                                   --output-format stream-json
                   │                                   --trust-tools=...)  → exec.Guard
                   ├─ approval.Gate (CLI y/n; 事前/結果) ─┐
                   └─ store(SQLite 正) ──> markdown(参照)  │
                   └─ memory.Recorder(optional) ─> unefeuille.Store
   すべての遷移・承認・出力は events として SQLite に永続化（RD-3）
```

## API and data-model changes

- 新規リポジトリ。公開 API 後方互換の制約はないが、ポート（AgentRuntime/LLMProvider/
  memory）は安定志向（COMP-002）。
- 追加依存（想定）：`modernc.org/sqlite`（永続化）。Ollama は HTTP（標準ライブラリ）。
  Une Feuille 連携は `unefeuille.Store` 契約にのみ依存（特定 Backend 非依存, COMP-001）。

## Error handling

- Runtime/Provider のエラー・タイムアウトは Engine が捕捉し、安全な終端状態へ遷移。
- 禁止 Git 操作・allowlist 外コマンドは `exec.Guard` が拒否し Event に記録。
- 承認却下は Rejected（terminal）。部分成果物は保持。

## Security considerations

- SEC-001：クラウド API（`IsCloud()==true`）呼び出しは PreExecApproval 必須。
- SEC-002：認証情報・秘密鍵を読まず、SQLite/Markdown/ログに平文で残さない。
  Provider の鍵は環境変数等から参照し、永続化対象に含めない。
- SEC-003：NAVETTE が直接起動するコマンドは `exec.Guard` の境界制御下に置く。Kiro CLI には
  `--trust-tools` を最小化して渡すが、これは起動オプションであり内部操作の完全制御は保証
  しない。Agent 実行の主たる安全担保は**実行環境の隔離**（§Execution isolation）とする。

### 保証範囲と未確認（Kiro CLI 内部操作の制御可能性）

- **保証できる範囲**：NAVETTE はプロセス起動の引数（`--no-interactive` /
  `--output-format stream-json` / `--trust-tools`）と、`exec.Guard` によるコマンド境界、
  作業ディレクトリ・環境変数・プロセスの生死（timeout/cancel）を制御できる。
- **保証できない範囲（現時点未確認）**：Kiro CLI プロセス内部で、信頼済みツールが起動した
  後の個々の内部操作を NAVETTE が逐一介入・取消しできるかは未確認。したがって初期 PoC は
  **実行前のタスク単位承認＋実行環境の制限**を基本とし（RD-2）、実行中の操作単位承認は
  Kiro CLI の制御機構（trust-tools 粒度・stream-json の承認イベント有無）を実機確認してから
  設計する。stream-json スキーマの詳細は実装着手時にイベント採取で確定する（RD-1）。

## Compatibility and migration

- COMP-001：Une Feuille 連携は `unefeuille.Store` 契約のみ。Une Feuille 不在でも動作。
- 既存リポジトリへの影響なし（独立リポジトリ, ADR-0001）。

## Observability

- 全状態遷移・承認・runtime 出力を `events` に記録。Markdown で人間可読ビューを生成。

## Implementation status — Milestone B（実装済み / fake Runtime のみ）

実 Kiro CLI は起動していない（Milestone C）。本 Milestone で実装・検証した要点：

- **承認の原子性**：`store.ApproveAndTransition` が承認行・承認イベント・状態遷移・遷移
  イベントを単一トランザクションで確定。状態不一致/重複承認を拒否し部分記録を残さない。
- **cancel/timeout の確実化**：終端状態は `context.WithoutCancel` ＋独立した短い timeout
  （5s）で永続化。runtime が成功を返しても `runCtx.Err()` があれば結果承認へ進まない。
  永続化失敗は `Outcome.PersistError` で呼び出し元へ明示（成功と誤報告しない）。
- **Runtime 出力保持**：`OutputEvent.Text` を 8 KiB 上限で保存（超過は `truncated` フラグ）。
  1 件ずつストリーム記録しメモリ無制限蓄積を回避。書込失敗時は note を残し当該バッチ中断。
  秘密情報は出力経路に載せない（SEC-002）。Milestone C 向けに `runtime.EventSink` /
  `StreamingRuntime`（JSONL 逐次 Emit）の最小 IF を用意。
- **承認バイパス防止**：CLI の `--approve-pre/--approve-result` は **TEST-ONLY で `--fake`
  必須**。Milestone B の `run` は `--fake` 必須（実 Runtime は Milestone C）。実 Runtime では
  結果確認前の自動承認を不可とする。結果承認時は成果物（作業コピー差分・実行ログ）の確認を
  促す（最小 IF）。
- **CLI 終了コード**：Succeeded=0 / run error=1 / awaiting approval=3 / rejected=4 /
  persistence failure=5。承認待ち・却下・失敗・永続化失敗を区別し、非成功を 0 にしない。
- **並行実行の安全性**：同一タスクへの並行 `RunTask` は状態機械＋原子的承認により二重
  Runtime 起動を防ぐ（`TestConcurrentRunNoDoubleRuntime`）。
- **CLIGate のキャンセル**：標準入力 Read は ctx で中断不可のため、buffered チャネルで
  reader goroutine を bound（cancel 後も stdin 入力/EOF で確実に終了、送信ブロックなし）。
  将来のデーモン前段ではキャンセル可能な入力源に差し替える。
- **RecoverInterrupted の責務**：Control Plane 状態の復旧のみ（Running→Indeterminate、
  自動再開なし）。子孫プロセス回収は Milestone C の隔離実装の責務。

## Test strategy

- 状態機械：許可/不許可遷移の単体テスト（R1 AC-4）。
- 永続化：登録→遷移→再起動復元、Running 中断→Indeterminate（R2）。
- Engine：timeout/cancel/MaxIterations の境界と Event 記録（R5）。
- Approval：事前/結果の区別、未承認の再起動後維持・非自動再開（R6）。fake 入力で検証。
- Runtime/Provider：インターフェースに fake 実装を与えた Engine テスト（実 CLI/Ollama なしで
  CI 可能）。kiroruntime は stream-json パーサをサンプル JSONL で単体テスト。
- exec.Guard：Git 操作・allowlist 外コマンドの拒否（R7）。
- Memory：Store nil（スキップ）/ memfake 設定の双方（R8）。
- `go build ./...` / `go test ./...` / `go vet ./...`（NFR-003）。

## Alternatives considered

- A: `--trust-all-tools` で Kiro CLI に委譲 → 無断実行を招き承認ゲートを損なうため却下。
- B: Markdown を状態の正とする → 構造化クエリ・整合性に不利。SQLite を正に（RD-3）。
- C: Une Feuille を必須依存に → PoC 進行を Une Feuille 完成に縛るため却下（R8）。
- D: 実行中の操作単位承認を初期から実装 → Kiro CLI 内部制御の未確認部分に依存するため、
  実機確認後に設計（RD-2, §保証範囲と未確認）。

## Risks and mitigations

- R-1: stream-json スキーマ未確定 → 実装着手時にイベント採取し、パーサをサンプル固定の
  単体テストで保護。スキーマ変更に備えパーサを分離。
- R-2: Kiro CLI 内部操作の完全制御が不可能 → 実行前承認＋環境隔離＋trust-tools 最小化で
  ブラスト半径を限定（§保証範囲と未確認）。
- R-3: タイムアウトで**ツール起動の子孫プロセスが残留**（NAV-TASK-001 で確認）→ ctx cancel
  後にプロセスグループ単位 kill ＋ bubblewrap 隔離環境ごとの破棄で回収。実装時に検証。
- R-4: Git 制限の抜け → allowlist＋.git 保護＋環境変数＋権限の多層で担保（RD-5）、Guard の
  拒否テストで固定。

## Open questions

- OQ-1: `kiro-cli --output-format stream-json` の具体的イベントスキーマ（型・承認/ツール
  イベントの有無）。実装着手時に実機でイベント採取して確定する（RD-1 に従い推測しない）。
- OQ-2: 反復上限の既定値。**初期値案：Control Plane の実行ループ上限 = 10 回**（タスク単位。
  実機の挙動を見て調整）。Agent 内部（Kiro CLI 内）の反復を Runtime 側で制限できるかは実機
  確認が必要で、Control Plane のループ回数とは別物として区別する（RD-4）。
- OQ-3: Git 制限の最小構成（環境変数 / ラッパ / 権限のどれを PoC で採用するか）（RD-5）。
