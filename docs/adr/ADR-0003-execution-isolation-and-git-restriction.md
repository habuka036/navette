# ADR-0003: 実行環境の隔離と DevContainer 内 Git 操作の制限

- Status: Accepted
- Date: 2026-10-14

## Context

NAVETTE は自律的に開発タスクを実行する Harness であり、Agent による副作用
（ファイル変更、コマンド実行、リポジトリ状態の変更）を伴う。制御されない Git 操作や
長時間・無制限の実行は、リポジトリ状態の破壊や暴走のリスクがある。

Workspace 共通ルールとして、DevContainer 内では commit / push / merge / reset /
checkout などリポジトリ状態を変更する Git 操作を行わない方針が定められている。

## Decision

NAVETTE は以下の隔離・制限を設ける。

- DevContainer 内でリポジトリ状態を変更する Git 操作（commit / push / merge / reset /
  checkout 等）を行わない。
- タスク実行にタイムアウト、キャンセル、反復回数の上限を設ける。
- Agent / Runtime が実行できる操作の範囲を Control Plane が制御する。

### 承認時の具体化事項（2026-10-14）

- Git 操作の禁止は steering（宣言的ルール）だけに依存しない。実行環境・権限による
  制限（例: 実行ユーザーの権限、環境変数による Git 無効化、作業ディレクトリの隔離、
  Runtime へ渡す許可コマンドの allowlist 等）を design で検討し、多層で担保する。

## Rationale

- Git 状態変更操作を禁止することで、自律実行による意図しないリポジトリ破壊を防ぐ。
  コミットなどの最終判断は人間が DevContainer 外で行う。
- タイムアウト・キャンセル・反復上限は、LLM / Agent の暴走や無限ループを境界で止める
  ための最小限の安全機構である。
- 代替案 (A) Git 操作を許可し hook で検閲：実装が複雑で漏れのリスクがあるため、初期は
  一律禁止とする。

## Consequences

- 利点：自律実行による副作用の最大ブラスト半径を限定できる。
- 制約：NAVETTE 自身は commit 等を行えないため、変更の確定は人間または DevContainer 外の
  プロセスに委ねる。
- 将来の影響：隔離の粒度（コンテナ単位・作業ディレクトリ単位）は PoC の知見を踏まえ
  後続 ADR で精緻化しうる。
