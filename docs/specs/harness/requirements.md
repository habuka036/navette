# NAVETTE — Requirements

- Status: Approved with conditions (OQ resolutions reflected 2026-10-14); Design in progress
- Date: 2026-10-14
- Spec location: `navette/docs/specs/harness/`
- Related ADRs: ADR-0001 (independent generic harness, Accepted),
  ADR-0002 (pluggable runtime/provider, Accepted),
  ADR-0003 (execution isolation & git restriction, Accepted),
  ADR-0004 (persistence & human approval, Accepted)

## Introduction

NAVETTE は、Kiro CLI や LLM を利用して開発タスクを実行・検証・管理する汎用的な
Development Harness である。Control Plane（タスク状態・実行履歴・制御ループ・承認）を
NAVETTE 自身が保持し、Agent Runtime と LLM Provider を差し替え可能にする。Go で実装し、
module path は `github.com/habuka036/navette`。

この requirements は初期 PoC の範囲を定義する。初期 Agent Runtime は Kiro CLI、初期
LLM Provider は Ollama、永続化は SQLite + Markdown、承認ゲートは CLI 対話とする。

## Goals

- 開発タスクを定義し、状態機械に沿って実行・追跡できる。
- Agent Runtime（初期 Kiro CLI）と LLM Provider（初期 Ollama）を交換可能にする。
- タスク状態と実行履歴を永続化し、再起動をまたいで保持する。
- タイムアウト・キャンセル・反復回数上限で実行を境界づける。
- 重要操作に人間の承認（CLI 対話）を課す。
- DevContainer 内でリポジトリ状態を変更する Git 操作を行わせない。
- Une Feuille が利用できなくても基本的なタスク実行・永続化が動作する。

## Non-goals

- Imagorium 固有ロジックの NAVETTE 本体への組み込み（ADR-0001; 汎用性維持）。
- OpenAI Provider / Arctic Tern Adapter の実装（将来の追加候補）。
- Web UI・多人数同時実行・分散実行（PoC 範囲外）。
- NAVETTE 自身による commit / push / merge 等の Git 状態変更（ADR-0003）。
- 高度なデータストア（外部 DB・キュー）。PoC は SQLite + Markdown（ADR-0004）。

## Assumptions

- Kiro CLI が実行環境に存在し、非対話で起動できる（起動規約は design/調査で確定）。
- Ollama が到達可能なエンドポイントで動作する（既定はローカル）。
- DevContainer 内で実行され、Git 状態変更操作は環境・権限でも制限しうる（ADR-0003）。

## Constraints

- Go 1.26 系。永続化は SQLite（pure-Go, CGO 不要）+ Markdown。
- クラウド API 呼び出しは明示承認が必要（OpenAI 等は将来 Provider）。
- Une Feuille 連携は optional。欠如時も基本機能が動く。
- DevContainer 内 Git 状態変更操作の禁止は steering のみに依存せず、実行環境・権限でも
  担保する（ADR-0003）。

## Requirements

### Requirement 1: タスクの定義と状態管理

**User Story:** 利用者として、開発タスクを定義し、その状態と履歴を追跡したい。

#### Acceptance Criteria

1. WHEN 利用者がタスク（目的・入力・制約）を登録する
   THE SYSTEM SHALL タスクを一意 ID 付きで永続化し、初期状態を記録する。
2. THE SYSTEM SHALL タスクの状態を決定的な状態機械で管理する
   （例: Pending → Running → AwaitingApproval → Approved/Rejected → Succeeded/Failed/
   Cancelled/TimedOut）。
3. WHEN タスクの状態が遷移する
   THE SYSTEM SHALL 遷移の履歴（時刻・前後状態・理由）を永続化する。
4. IF 定義された状態機械で許可されない遷移が要求される
   THEN THE SYSTEM SHALL その遷移を拒否し、状態を変更しない。

### Requirement 2: 実行履歴と成果物の永続化（SQLite + Markdown）

**User Story:** 利用者として、再起動後もタスクの状態・履歴・成果物を失わず参照したい。

#### Acceptance Criteria

1. THE SYSTEM SHALL タスク状態と状態遷移・実行イベントを SQLite に永続化する。
2. THE SYSTEM SHALL 人間が読む実行ログ・成果物を Markdown として永続化する。
3. WHEN プロセスが再起動する
   THE SYSTEM SHALL 永続化済みのタスク状態・履歴を復元し、整合した状態から再開可能にする。
4. IF タスクが実行途中でプロセスが異常終了する
   THEN THE SYSTEM SHALL 再起動時に当該タスクを「不確定（要再評価）」として扱い、
   成功・失敗へ暗黙に遷移させない。

### Requirement 3: Agent Runtime の差し替え（初期 Kiro CLI）

**User Story:** 利用者として、Agent の実行方式を交換できるようにしたい。初期は Kiro CLI を
使いたい。

#### Acceptance Criteria

1. THE SYSTEM SHALL `AgentRuntime` インターフェースを定義し、Control Plane が実装を
   注入できるようにする。
2. THE SYSTEM SHALL Kiro CLI を用いる `AgentRuntime` 実装を提供する。
3. WHEN Control Plane がタスクを実行する
   THE SYSTEM SHALL 注入された `AgentRuntime` を介して実行し、Runtime 固有の詳細を
   Control Plane に漏らさない。
4. IF `AgentRuntime` が未設定である
   THEN THE SYSTEM SHALL タスク実行開始時に明示エラーを返す（暗黙に no-op しない）。

### Requirement 4: LLM Provider の差し替え（初期 Ollama）

**User Story:** 利用者として、LLM Provider を交換できるようにしたい。初期はローカルの
Ollama を優先したい。

#### Acceptance Criteria

1. THE SYSTEM SHALL `LLMProvider` インターフェースを定義し、実装を注入できるようにする。
2. THE SYSTEM SHALL Ollama を用いる `LLMProvider` 実装を提供する。
3. IF 設定された Provider がクラウド API（例: OpenAI）を呼ぶ
   THEN THE SYSTEM SHALL 事前に人間の承認を要求する（Requirement 6）。
4. WHEN Provider が応答しない・エラーを返す
   THE SYSTEM SHALL タイムアウトし、タスクを失敗またはキャンセル状態へ安全に遷移させる。

### Requirement 5: 実行境界（タイムアウト・キャンセル・反復上限）

**User Story:** 利用者として、暴走や無限ループを境界で止めたい。

#### Acceptance Criteria

1. THE SYSTEM SHALL タスク実行ごとにタイムアウトを設定でき、超過時に実行を打ち切り
   TimedOut 状態へ遷移させる。
2. WHEN 利用者がキャンセルを要求する
   THE SYSTEM SHALL 実行中のタスクへキャンセルを伝播し（context 経由）、Cancelled 状態へ
   遷移させる。
3. THE SYSTEM SHALL 反復（Agent/LLM の呼び出し反復）回数の上限を設定でき、超過時に実行を
   打ち切る。
4. IF タイムアウト・キャンセル・反復上限で打ち切られた
   THEN THE SYSTEM SHALL 打ち切り理由を履歴に記録し、部分成果物を保持する。

### Requirement 6: 人間の承認ゲート（CLI 対話）

**User Story:** 利用者として、重要操作を Agent に無断で実行させず、承認を挟みたい。
また、実行の前と後で承認の意味を区別したい。

#### Acceptance Criteria

1. THE SYSTEM SHALL 承認を 2 種類に区別する：
   (a) **実行前承認**（タスク/重要操作を開始してよいか）、
   (b) **実行結果承認**（実行された結果を受理・確定してよいか）。
   各承認は独立に記録され、一方の承認が他方を含意しない。
2. WHEN タスクが重要操作（破壊的コマンド・外部副作用・クラウド API 呼び出し等）に到達する
   THE SYSTEM SHALL AwaitingApproval 状態へ遷移し、CLI 対話で実行前承認/却下を求める。
3. WHILE タスクが実行前承認待ちである
   THE SYSTEM SHALL 承認が得られるまで当該重要操作を実行しない。
4. WHEN CLI が切断される、またはプロセスが再起動する
   THE SYSTEM SHALL 当該タスクの未承認状態を維持し、承認済み・失敗へ暗黙に遷移させない。
   AND SHALL 未承認の操作を自動的に再開しない（再開には改めて承認を要する）。
5. WHEN 人間が承認/却下する
   THE SYSTEM SHALL その決定と決定者・時刻・承認種別(a/b)を履歴に記録し、対応する状態へ
   遷移する。
6. IF 非対話実行中に承認が必要になった
   THEN THE SYSTEM SHALL 無断実行せず、安全に AwaitingApproval へ移行して停止する
   （承認が得られるまで重要操作を行わない）。

### Requirement 7: 実行環境の隔離と Git 操作の制限

**User Story:** 運用者として、自律実行による副作用の範囲を限定し、Git 状態の破壊を防ぎたい。

#### Acceptance Criteria

1. THE SYSTEM SHALL DevContainer 内でリポジトリ状態を変更する Git 操作
   （commit / push / merge / reset / checkout 等）を実行しない。
2. THE SYSTEM SHALL この制限を宣言的ルール（steering）だけに依存せず、実行環境・権限
   （実行コマンドの allowlist、環境変数、作業ディレクトリ隔離等）でも担保する。
3. IF Agent/Runtime が禁止された Git 操作を要求する
   THEN THE SYSTEM SHALL その操作を拒否し、理由を履歴に記録する。

### Requirement 8: Une Feuille 連携の任意性（PoC で必須依存にしない）

**User Story:** 利用者として、Une Feuille（記憶基盤）が未完成でも NAVETTE の基本機能を
使いたい。

#### Acceptance Criteria

1. THE SYSTEM SHALL 初期 PoC において Une Feuille を必須依存とせず、NAVETTE 単独で
   タスク実行・永続化（SQLite + Markdown）が動作する。
2. IF Une Feuille の `Store` が設定されていない
   THEN THE SYSTEM SHALL 記憶連携をスキップし、タスク実行・永続化を通常どおり継続する。
3. WHEN Une Feuille の `Store` が設定されている
   THE SYSTEM SHALL タスクの記憶（ログ・成果物の要約等）を Une Feuille 契約経由で保存する。
4. THE SYSTEM SHALL Une Feuille の有無でタスク状態機械の基本挙動を変えない。

## Non-functional requirements

- NFR-001: すべての実行境界（タイムアウト・キャンセル）は `context.Context` で伝播する。
- NFR-002: 永続化は単一ファイル SQLite（pure-Go）+ Markdown。追加の外部サービスを要しない。
- NFR-003: `go build ./...` / `go test ./...` / `go vet ./...` がパスする。
- NFR-004: Control Plane は Runtime/Provider 実装詳細に依存しない（インターフェース境界）。

## Security and privacy requirements

- SEC-001: クラウド API 呼び出しは明示承認なしに行わない（Requirement 4/6）。
- SEC-002: 認証情報・秘密鍵を読み取らず、ログ・Markdown・SQLite に平文で記録しない。
- SEC-003: Agent/Runtime が実行可能な操作は Control Plane が制御し、禁止操作を拒否する。

## Compatibility requirements

- COMP-001: Une Feuille 連携は `github.com/habuka036/millefeuille/unefeuille` の `Store`
  契約にのみ依存し、特定 Backend に依存しない。
- COMP-002: 新規リポジトリのため既存公開 API との後方互換制約はないが、PoC 以降の
  インターフェース（AgentRuntime/LLMProvider/Store 連携）は安定を志向する。

## Resolved decisions (承認時に確定, 2026-10-14)

- RD-1 (旧 OQ-1): Kiro CLI の非対話実行方式は、現在の開発環境で実際の CLI を調査して確定する。
  未確認のオプションを前提に実装しない（design フェーズで実機調査）。
- RD-2 (旧 OQ-2): 重要操作の判定は、まず「実行前のタスク単位の承認」と「実行環境の制限」を
  基本とする。実行中の操作単位承認は、Kiro CLI が提供する制御機構を確認してから設計する。
- RD-3 (旧 OQ-3): SQLite を状態・イベント履歴の正（source of truth）とし、Markdown は
  人間向けの参照・レビュー用成果物とする。
- RD-4 (旧 OQ-4): 反復上限は Control Plane の実行ループ回数で定義する。Agent 内部の反復回数は
  Runtime が制御可能な場合のみ別途制限する。
- RD-5 (旧 OQ-5): Git 操作制限は steering だけに依存しない。対象リポジトリの書き込み範囲、
  Git メタデータの保護、実行環境の隔離を優先して検討する。

### Design に反映する追加事項

- 実行前承認と実行結果承認を区別する（Requirement 6 AC-1）。
- Kiro CLI 内部の操作を NAVETTE がどこまで制御できるか、保証できる範囲／できない範囲を
  design に明記する。
- 非対話実行中に承認が必要になった場合、無断実行せず安全に停止または承認待ちへ移行する
  （Requirement 6 AC-6）。
- CLI 切断・再起動時には未承認の操作を自動再開しない（Requirement 6 AC-4）。
- 初期 PoC では Une Feuille を必須依存とせず、NAVETTE 単独で動作可能にする
  （Requirement 8 AC-1）。
