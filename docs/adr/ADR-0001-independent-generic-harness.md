# ADR-0001: NAVETTE を独立した汎用 Development Harness とする

- Status: Accepted
- Date: 2026-10-14

## Context

Imagorium の開発速度向上のため、Kiro CLI や LLM を利用して開発タスクを実行・検証・
管理する Development Harness「NAVETTE」が必要になった。

NAVETTE を Imagorium 専用ツールとして組み込むか、汎用的な独立ツールとするかを決める
必要があった。Imagorium 専用化は初期の設計を単純化できる一方、他プロジェクトへの
再利用性を失い、Imagorium の実装詳細に結合するリスクがある。

Workspace 上で Imagorium / MILLEFEUILLE と同居しているが、これはソフトウェアとしての
密結合を意味しない（各リポジトリは独立）。

## Decision

NAVETTE は Imagorium 専用ではなく、汎用的な Development Harness として
`github.com/habuka036/navette` を module path とする独立リポジトリで実装する。
Go で実装する。

## Rationale

- 汎用化により Imagorium 以外のプロジェクトでも Harness を再利用できる。
- Imagorium の実装詳細に結合しないことで、Imagorium 側の変更が NAVETTE に波及しない。
- Go 採用理由：Workspace の他コンポーネント（MILLEFEUILLE）と言語を揃え、単一バイナリ
  配布と DevContainer 内実行が容易。
- 代替案 (A) Imagorium 専用ツール化：初期は単純だが再利用性と疎結合を失うため不採用。

## Consequences

- 利点：汎用 Harness として他プロジェクトへ展開可能。Imagorium と疎結合を保てる。
- 制約：Imagorium 固有の都合を NAVETTE 本体に持ち込めない。Imagorium 固有部分は
  Adapter や設定として外から注入する必要がある。
- 将来の影響：Agent Runtime / LLM Provider / Memory Backend を差し替え可能にする後続
  ADR（ADR-0002 以降）の前提となる。
