# ADR-0002: Agent Runtime と LLM Provider を差し替え可能にする

- Status: Accepted
- Date: 2026-10-14

## Context

NAVETTE は Kiro CLI や LLM を利用して開発タスクを実行する。しかし特定の Agent
実行方式や特定の LLM Provider に固定すると、将来の選択肢（別の CLI、別の API、
ローカル実行）を取り込めなくなる。

初期の優先事項は以下と決まっている。

- 初期 Agent Runtime は Kiro CLI。
- 初期 LLM Provider は Ollama を優先する。
- OpenAI API は将来的な追加 Provider として扱う。
- Arctic Tern は将来の Adapter 候補とする。

## Decision

NAVETTE は Control Plane（タスク状態・実行履歴・制御ループ・承認）を自身で保持し、
以下の 2 つを差し替え可能なインターフェースとして定義する。

- `AgentRuntime`：開発タスクを実行する Agent の実行方式。初期実装は Kiro CLI。
- `LLMProvider`：LLM 呼び出しの抽象。初期実装は Ollama。OpenAI は将来追加。

具体的なインターフェース定義は NAVETTE の Feature Spec（design）で確定する。

## Rationale

- Control Plane を NAVETTE が保持することで、Runtime / Provider を差し替えても
  タスク状態管理・承認・制御ループの一貫性を保てる。
- Ollama を初期優先とすることで、クラウド API を呼ばずローカルで PoC を回せる
  （クラウド API 呼び出しは明示承認が必要という制約とも整合）。
- 代替案 (A) Kiro CLI / 特定 Provider に直結：初期は単純だが交換性を失うため不採用。

## Consequences

- 利点：Runtime / Provider を独立して追加・交換できる。ローカル優先で PoC を回せる。
- 制約：インターフェースを両者に被せるため、初期実装でも抽象レイヤの実装コストが生じる。
- 将来の影響：OpenAI Provider、Arctic Tern Adapter を追加する際は本 ADR の方針に従い、
  既存インターフェースを破壊しない形で拡張する。
