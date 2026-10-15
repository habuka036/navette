# NAVETTE — Tasks

- Status: Draft (awaiting review); implementation NOT started
- Date: 2026-10-14
- Spec location: `navette/docs/specs/harness/`
- Inputs: `requirements.md` (Approved), `design.md` (revised, awaiting review)
- Module path: `github.com/habuka036/navette`

実装順序の原則：

- 早期に実用化する。必要以上の抽象化・将来機能の先行実装はしない。
- 「NAVETTE 単独でタスクを実行し状態を永続化できる」最小の縦断スライスを最優先に通す。
- Une Feuille は optional。無しで動くことを先に成立させる。
- 実機依存（Kiro CLI の stream-json / Ollama / Git 隔離）は、推測せず**実機実験で確定**して
  から実装する。実験は秘密情報を含まない一時ディレクトリまたは使い捨て環境で行う。
- テスト先行。Runtime/Provider は fake を与えて実 CLI/Ollama なしで CI 可能にする。

状態記号：`[ ]` 未着手 / `[~]` 実行中 / `[x]` 完了 / `[!]` ブロック。

## Milestone A: 実機調査（コード実装の前提確定、推測しない）

- [ ] NAV-TASK-001 Kiro CLI stream-json スキーマのサンプル採取（実験）
  - Requirements: R3, OQ-1(RD-1)
  - Design: §kiroruntime, Open questions OQ-1
  - Depends on: None
  - 作業: 使い捨て一時ディレクトリで `kiro-cli chat "echo hello" --no-interactive
    --output-format stream-json` 等を実行し、出力 JSONL の実イベント型・フィールドを採取。
    秘密情報を扱わない。採取結果を `docs/specs/harness/notes/stream-json-samples.md` に記録。
  - Completion criteria:
    - stream-json の最小スキーマ（イベント型・終了判定・エラー表現）が文書化される。
    - 承認/ツール関連イベントの有無が確認され、操作単位承認の可否判断材料が揃う。

- [ ] NAV-TASK-002 反復上限と Git 隔離の最小構成の実機確認（実験）
  - Requirements: R5(RD-4), R7(RD-5)
  - Design: §Control loop, §Execution isolation, OQ-2/OQ-3
  - Depends on: NAV-TASK-001
  - 作業: 使い捨て環境で、(a) Control Plane ループ上限の初期値 10 の妥当性、(b) 作業ツリー
    のみ書込可能・`.git` 保護・秘密非露出を満たす最小隔離構成（一時 copy / 権限 / dir 制限）を
    比較し、実効性をテストできる形を 1 つ選定。
  - Completion criteria:
    - 反復上限の既定値が確定（初期案 10）。Agent 内部反復との区別が明記。
    - PoC で採用する最小隔離構成と、その実効性テスト方法が決まる。

## Milestone B: NAVETTE 単独の最小縦断スライス（Une Feuille 無しで動く）

> 実装状況: NAV-TASK-010〜017 完了（fake Runtime のみ、実 Kiro CLI 未起動）。
> コードレビュー補修を反映済み（承認の原子的確定、cancel/timeout の確実化、Runtime 出力
> 保持 ＋ `EventSink`/`StreamingRuntime` IF、`--approve-*` の TEST-ONLY 化、CLI 終了コード、
> 並行二重起動防止、CLIGate キャンセル対応、RecoverInterrupted の責務明記）。詳細は
> design.md「Implementation status — Milestone B」を参照。

- [x] NAV-TASK-010 go.mod とパッケージ骨格
  - Requirements: NFR-002/003
  - Design: §パッケージ構成
  - Depends on: None（Milestone A と並行可）
  - Files: `go.mod`, `cmd/navette/main.go`, `internal/*/` の空骨格
  - Completion criteria: `go build ./...` が通る。module path は github.com/habuka036/navette。

- [x] NAV-TASK-011 ドメイン型と状態機械（遷移表）
  - Requirements: R1
  - Design: §Domain model, §State machine
  - Depends on: NAV-TASK-010
  - Files: `internal/task/task.go`, `state.go`, `transition.go`, `transition_test.go`
  - Completion criteria:
    - 最小状態（Pending/AwaitingPreApproval/Running/AwaitingResultApproval/Succeeded/
      Failed/Cancelled/TimedOut/Rejected/Indeterminate）と許可遷移表。
    - 許可外遷移を拒否するテストが通る（テスト先行）。

- [x] NAV-TASK-012 SQLite 永続化（状態・イベントの正）
  - Requirements: R2(RD-3)
  - Design: §Persistence
  - Depends on: NAV-TASK-011
  - Files: `internal/store/sqlite.go`, `migrations/`, `sqlite_test.go`
  - Completion criteria:
    - tasks/runs/events/approvals テーブル。WAL + マイグレーション。
    - **状態遷移＋イベント記録を単一トランザクション**で書く API。
    - 登録→遷移→再読込で復元、Running 中断→Indeterminate を再現するテスト。

- [x] NAV-TASK-013 AgentRuntime ポートと fake runtime
  - Requirements: R3, NFR-004
  - Design: §Ports
  - Depends on: NAV-TASK-011
  - Files: `internal/runtime/runtime.go`, `internal/runtime/fakeruntime/fake.go`
  - Completion criteria:
    - `AgentRuntime` interface。未設定時に実行開始で明示エラー（暗黙 no-op しない）テスト。
    - fake runtime で Engine を実 CLI 無しに回せる。

- [x] NAV-TASK-014 Engine 制御ループ（timeout/cancel/反復上限）
  - Requirements: R5(RD-4), NFR-001
  - Design: §Control loop
  - Depends on: NAV-TASK-012, NAV-TASK-013
  - Files: `internal/engine/engine.go`, `bounds.go`, `engine_test.go`
  - Completion criteria:
    - timeout→TimedOut、cancel→Cancelled、MaxIterations(既定 10)超過→打ち切り、を fake で検証。
    - 打ち切り理由を Event に記録し部分成果物を保持。

- [x] NAV-TASK-015 承認ゲート（事前/結果の分離, CLI 対話）
  - Requirements: R6
  - Design: §Approval model, §State machine
  - Depends on: NAV-TASK-012, NAV-TASK-014
  - Files: `internal/approval/approval.go`, `approval_test.go`
  - Completion criteria:
    - 事前承認なしに Runtime を起動しないテスト。
    - 結果承認を事前承認と独立に記録。承認/却下で Succeeded/Rejected。
    - 未承認状態が再起動後も維持され、自動再開しない（永続化込み）テスト。
    - 承認決定の記録が状態遷移と同一 SQLite トランザクション。

- [x] NAV-TASK-016 Markdown 参照ビュー生成（SQLite から派生）
  - Requirements: R2(RD-3)
  - Design: §Persistence
  - Depends on: NAV-TASK-012
  - Files: `internal/store/markdown.go`, `markdown_test.go`
  - Completion criteria: タスクサマリ・実行ログ・承認履歴を Markdown 出力（正ではない派生）。

- [x] NAV-TASK-017 cmd/navette CLI（登録・実行・承認・一覧）
  - Requirements: R1, R5, R6
  - Design: §パッケージ構成, Data flow
  - Depends on: NAV-TASK-014, NAV-TASK-015
  - Files: `cmd/navette/main.go`
  - Completion criteria: fake runtime で「タスク登録→事前承認→実行→結果承認→終端」を
    end-to-end で CLI から実行でき、状態・履歴が SQLite/Markdown に残る。

## Milestone C: 実機 Runtime / Provider / 隔離（Milestone A/B 完了後）

> 実装状況（最小スライス＋レビュー補修2巡, 2026-10-14）：実 Kiro CLI を bubblewrap 隔離下で
> 実行する経路が成立。レビュー2巡目で以下を追加対応（詳細は notes/milestone-c-sandbox.md
> 「追加補修」）：
> - 成果物保存/記録/検証を Engine の ResultFinalizer として結果承認後〜Succeeded 前に実行。失敗は
>   Failed にし CLI は非0。失敗時は作業コピー・成果物を保持（回復可能）。
> - 状態機械に Finalizing を追加し「承認→Finalizing→(保存+検証)→Succeeded」に再設計。Succeeded 後の
>   不一致判明を排除。承認記録と遷移の原子性は維持。
> - プロセス回収を parse/EventSink 失敗時にも能動発火。cmd.Wait() とグループ終了を同一視せず、
>   SIGTERM 無視子孫を SIGKILL で確実停止（PID 追跡テスト）。
> - RemoveWorkCopy を厳格化（prefix 一致・temp 直下・非 symlink・ディレクトリ検証）。

> 実装状況（最小スライス＋レビュー補修1巡, 2026-10-14）：実 Kiro CLI を bubblewrap 隔離下で
> 実行する経路が成立。補修で以下を対応済み（詳細は notes/milestone-c-sandbox.md「実装結果」）：
> - 成果物を run 単位の永続ディレクトリへ保存し SQLite(runs: artifact_dir/artifact_hash)で管理。
>   作業コピーは成果物保存後にのみ削除（却下・中断でも成果物を保持）。実リポジトリ反映なし。
> - EventSink をエラー返却可能化。出力永続化失敗を Engine へ伝播し、結果承認/Succeeded へ進めない。
> - プロセスグループ回収を修正（exited 通知＋猶予満了で確実に SIGKILL）。SIGTERM 無視子プロセスの
>   テスト追加。
> - bubblewrap を `--clearenv` 既定にし HOME/PATH＋allowlist のみ設定。ホスト API キー非継承をテスト。
> - 結果承認で unified diff を提示。差分/フィンガープリント取得失敗時は承認不可（fail-closed）。
>   承認時の作業コピー fingerprint と保存成果物の一致を検証。
> - 作業コピー生成で .git/VCS・秘密ファイル(.env/鍵/credentials)除外、コピーサイズ上限、削除対象
>   パス検証。
> - stream-json の終了イベント重複・矛盾を ErrProtocol で fail-closed。Execute のイベント収集を修正。

- [x] NAV-TASK-020 kiroruntime（stream-json パーサ, 実機スキーマ基準）
  - Requirements: R3
  - Design: §kiroruntime
  - Depends on: NAV-TASK-001, NAV-TASK-013
  - Files: `internal/runtime/kiroruntime/kiro.go`, `kiro_test.go`
  - Completion criteria:
    - NAV-TASK-001 の採取サンプルを固定入力に stream-json パーサを単体テスト。
    - `--no-interactive --output-format stream-json --trust-tools=` 最小化で起動。
    - 事前承認後のみ起動。内部操作単位承認は実装しない（未確認範囲として明記）。

- [~] NAV-TASK-021 exec.Guard（NAVETTE 直接起動コマンドの境界制御）
  - Milestone C 最小スライスでは bubblewrap 隔離（NAV-TASK-022）を主たる境界とし、exec.Guard
    は未実装のまま保留。実リポジトリ書込は作業コピー隔離で防止済み。将来、サンドボックス外で
    NAVETTE が直接コマンドを起動する経路が出た場合に実装する。
  - Requirements: R7(RD-5)
  - Design: §Execution isolation
  - Depends on: NAV-TASK-002
  - Files: `internal/exec/guard.go`, `guard_test.go`
  - Completion criteria:
    - NAVETTE が直接起動するコマンドの Git 状態変更・allowlist 外を拒否し Event 記録。
    - 制御範囲が「直接起動コマンド境界」に限ることを doc/テストで明示（子プロセス非保証）。

- [x] NAV-TASK-022 隔離実行環境（作業ツリー限定・.git 保護・秘密非露出）
  - 実装: `internal/sandbox`（bubblewrap。work copy のみ書込・refs ro-bind・tmpfs /tmp・
    専用 HOME・SSH_AUTH_SOCK unset・share-net・die-with-parent・fail-closed）。実機 E2E で
    境界確認済み。詳細は notes/milestone-c-sandbox.md「実装結果」。
  - Requirements: R7(RD-5)
  - Design: §Execution isolation（主たる担保）
  - Depends on: NAV-TASK-002, NAV-TASK-021
  - Files: `internal/exec/isolation.go`, `isolation_test.go`
  - Completion criteria:
    - 書込可能を `Constraints.WorkDir` 配下に限定。Imagorium 3 リポジトリ読み取り専用。
    - `.git` 保護、認証情報/SSH Agent/Docker Socket 非露出。
    - 隔離未成立なら実リポジトリへの自律書き込みを許可しない動作をテスト。

- [ ] NAV-TASK-023 ollamaprovider（ローカル LLM）
  - Requirements: R4
  - Design: §ollamaprovider
  - Depends on: NAV-TASK-014
  - Files: `internal/provider/provider.go`, `internal/provider/ollamaprovider/ollama.go`
  - Completion criteria:
    - `LLMProvider` interface（`IsCloud()==false`）。タイムアウト/エラーで安全遷移。
    - クラウド Provider は `IsCloud()==true` で事前承認必須（SEC-001）。

## Milestone D: Une Feuille 連携（optional, 最後）

- [ ] NAV-TASK-030 memory.Recorder（unefeuille.Store を optional 連携）
  - Requirements: R8, COMP-001
  - Design: §Memory integration
  - Depends on: NAV-TASK-012
  - Files: `internal/memory/memory.go`, `memory_test.go`
  - Completion criteria:
    - Store nil ならスキップし基本機能継続。memfake 設定時は Run ログ/成果物要約を Save。
    - Une Feuille 有無で状態機械の基本挙動が変わらないテスト。

## Traceability

| Requirement | Design | Task | Test |
|---|---|---|---|
| R1 状態機械 | Domain/State machine | NAV-TASK-011 | transition_test |
| R2 永続化(SQLite 正/MD 参照) | Persistence | NAV-TASK-012,016 | sqlite_test, markdown_test |
| R3 AgentRuntime(Kiro) | Ports/kiroruntime | NAV-TASK-013,020 | fake, kiro_test |
| R4 LLMProvider(Ollama) | ollamaprovider | NAV-TASK-023 | provider test |
| R5 実行境界(RD-4) | Control loop | NAV-TASK-014 | engine_test |
| R6 承認(事前/結果) | Approval model | NAV-TASK-015 | approval_test |
| R7 隔離/Git(RD-5) | Execution isolation | NAV-TASK-021,022 | guard_test, isolation_test |
| R8 Une Feuille 任意 | Memory integration | NAV-TASK-030 | memory_test |
| OQ-1/2/3 実機確定 | §調査済み前提/OQ | NAV-TASK-001,002 | notes 記録 |

## 実行順序まとめ

1. Milestone A（実機調査; NAV-TASK-001 → 002）と NAV-TASK-010 を並行で開始可。
2. Milestone B（単独縦断）：011 → 012 → 013 → 014 → 015 →（016）→ 017。
   ここで **NAVETTE 単独 + fake runtime** のタスク実行ループが成立（最初の実用形）。
3. Milestone C（実機）：020（←001 必須）/ 021 / 022（←002 必須）/ 023。
4. Milestone D（Une Feuille 連携, optional）：030。

次回承認後、Milestone A の実機実験（NAV-TASK-001）から着手する想定。本 tasks.md 承認までは
機能実装に着手しない。
