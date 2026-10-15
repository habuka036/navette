# NAVETTE Milestone A — 実機実験結果

- Date: 2026-10-14
- Environment: DevContainer, `kiro-cli 2.21.1`, Linux, user `developer` (uid 1000)
- 実験方法: 使い捨て一時ディレクトリ（`/tmp/navette-exp-*`, `/tmp/iso*-*`）で実施。
  秘密情報・実リポジトリ（Imagorium / MILLEFEUILLE）には一切触れていない。実験後、一時
  ディレクトリと起動プロセスはクリーンアップ済み。

本ノートは **確認できた事実（Confirmed）** と **未確認・要再検証（Unconfirmed）** を区別する。

---

## NAV-TASK-001: Kiro CLI 非対話実行

### Confirmed（確認できた事実）

1. **stream-json は v2/v3 エンジン必須。**
   `--output-format stream-json` を既定（v1）で使うと失敗する。エラー:
   `--output-format stream-json is not supported on the v1 engine. Pass --agent-engine v2 (or v3).`
   → 起動コマンドは `kiro-cli chat <prompt> --no-interactive --output-format stream-json
   --agent-engine v2` を基本とする。

2. **stream-json のスキーマ（JSON Lines, 1 行 1 イベント）。**
   各行は `{"type": <string>, "data": <object>}` のエンベロープ。観測した `type`:
   - `runStarted`: `data.payloadSchema="acp"`, `acpProtocolVersion`, `engine`
   - `metadata`: `data.sessionId`, `contextUsagePercentage`, 終盤に `meteringUsage[]`
     (`value`/`unit`/`unitPlural`), `turnDurationMs`
   - `sessionUpdate`: `data.update.sessionUpdate` が
     - `agent_message_chunk`: `content.{type:"text", text}`（逐次チャンク）
     - `tool_call`: `toolCallId`, `title`, `kind`(例 `edit`), `content[]`（`diff` は
       `path`/`oldText`/`newText`）, `locations[]`, `rawInput`, `_meta.kiro.toolName`
     - `tool_call_update`: `toolCallId`, `status`(例 `completed`), `rawOutput`
   - `runFinished`: `data.status`(例 `success`), `stopReason`(例 `end_turn`),
     `finalText`, `finalTextTruncated`
   - `runError`: `data.stage`(例 `engine`), `data.message`（stdout にも 1 行で出る）

3. **終了コードと出力形式。**
   - 正常終了: exit `0`。エラー行は stdout に `runError` として、同時に stderr にも
     人間可読メッセージが出る場合がある。
   - 無効な `--model` 指定: stderr に `[warn] failed to set model ...` が出るが、既定に
     フォールバックして実行継続し **exit 0**。→ **exit code だけで成否判定しない**。
     `runFinished.status` と `runError` の有無で判定する。

4. **タイムアウト/キャンセル（親プロセス）。**
   SIGTERM で打ち切ると exit `124`（timeout 由来）。通常終了後・SIGTERM 後ともに、残留
   `kiro-cli` プロセスは観測されなかった（親プロセスはクリーンに終了する）。

### Unconfirmed / 要再検証・設計反映が必要な事実

5. **【重要】`--trust-tools=`（空＝信頼ツールなし）でもツールが実行された。**
   非対話モードで「ファイルを作成して」と依頼し `--trust-tools=` を渡したが、エージェントは
   `write` ツールを実行し **実際にファイルを作成した**。空の信頼リストは非対話モードでの
   ツール実行を **ブロックしなかった**。
   → **`--trust-tools` を NAVETTE の承認ゲートの根拠にできない。** 承認は NAVETTE 側の
   「実行前のタスク単位承認」で担保し、安全性の主たる担保は **実行環境の隔離** とする。
   （非対話モードでの承認/ブロックの正確な条件は未確認。`--trust-all-tools` 無し・
   allowlist 指定時の挙動差は追加検証が必要。）

6. **【重要】ツールが起動した子孫プロセスは親の終了後も残りうる。**
   信頼ツール経由で `sleep 300 &`（バックグラウンド）を起動させたところ、エージェント自身が
   「シェルツールはコマンド行完了で戻るため、プロセスは独立して走り続ける」と報告した。
   → **プロセスツリー kill だけでは子孫プロセスのクリーンアップを保証できない。** タイムアウト/
   キャンセル時のリソース回収は、プロセスグループ/セッション単位の kill や隔離環境ごとの破棄
   など、追加手段が必要（実装時に方式確定）。
   - 本実験は `pgrep` 不在のため残留確認が不完全。実装時にプロセスグループ単位の制御で
     再検証する。

7. **stream-json の全 `type` の網羅は未確認。** 失敗系（権限拒否・ツール失敗・
   コンテキスト超過・キャンセル時の最終イベント）での `runFinished.status` 値や追加 `type` は
   未採取。実装時にパーサをサンプル駆動で拡張する。

---

## NAV-TASK-002: 実行環境の隔離方式

### 利用可能な隔離プリミティブ（Confirmed）

- `bwrap`（bubblewrap）**利用可能**。非特権ユーザー名前空間が有効（`unshare --user
  --map-root-user` が uid 1000 で成功）。→ **Docker Socket 公開・特権コンテナ不要**で隔離可能。
- `unshare` / `setpriv` / `chattr` あり。`firejail` は無し。
- `/lib64` は存在しない（bind 対象から除外。`/usr` `/lib` `/bin` `/etc` を ro-bind）。

### bubblewrap で検証した隔離（Confirmed）

使い捨て環境で以下を 1 コマンドで検証し、要件を満たすことを確認した。

| 要件 | 方式 | 結果 |
|---|---|---|
| 対象作業ディレクトリのみ書込可 | `--bind <work> /sandbox/work` | work への書込 OK |
| 対象外リポジトリへの書込禁止 | `--ro-bind <repo> /sandbox/repo` | repo への書込 `Read-only file system` で拒否 |
| `.git` メタデータ保護 | repo を ro-bind（`.git` 含む） | `.git/HEAD` 書込拒否 |
| ネットワーク非到達 | `--unshare-net` | ネットワーク遮断 |
| ホスト認証情報/home 非露出 | home を bind しない | `/home` がサンドボックス内に存在しない |

ホスト側 repo の内容は実験後も不変（`tracked.txt`/`.git/HEAD` 変化なし）であることを確認。

### 提案する PoC 最小隔離構成

- **bubblewrap を主たる隔離手段**とする（非特権・Docker Socket 不要）。
  - 書込可能: `Constraints.WorkDir` を `--bind` で 1 箇所のみ。
  - それ以外（Imagorium 3 リポジトリ含む参照対象）は `--ro-bind` で読み取り専用。
  - `--unshare-net` で既定ネットワーク遮断（LLM/Provider 通信が要るものは別途明示穴あけ）。
  - home / SSH Agent / Docker Socket / 認証情報を bind しない。
- **多層防御**：bubblewrap に加え、`exec.Guard`（NAVETTE 直接起動コマンドの Git 操作拒否）と
  作業ツリー外書込の検知を併用。steering や allowlist 単独を安全性の根拠にしない。
- **隔離が実効的に成立しない場合、実リポジトリへの自律的書込を許可しない**（安全側に倒す）。

### Unconfirmed / 要再検証

- `--unshare-net` 下での TCP egress 遮断は、busybox sh の制約で `/dev/tcp` 検証が不完全。
  実装時に実 TCP クライアントで再確認する。
- LLM Provider（Ollama 等）通信が必要な場合のネットワーク穴あけ方式（ローカルソケットのみ
  通す等）は未設計。
- bubblewrap 内で `kiro-cli` を動かした場合の、子孫プロセス残留（NAV-TASK-001 #6）の
  抑止効果（隔離破棄でまとめて回収できるか）は未検証。実装時に確認。
- DevContainer 内で bubblewrap を使う際の seccomp/権限の相互作用（devcontainer.json は
  `--security-opt=seccomp=unconfined` を付与済み）の影響は未精査。

---

## 設計への反映（要点）

- kiroruntime 起動は `--agent-engine v2 --output-format stream-json --no-interactive` を基本に。
- 成否判定は exit code ではなく `runFinished.status` / `runError` を解析。
- 承認ゲートは NAVETTE 側の実行前タスク単位承認で担保（`--trust-tools` に依存しない）。
- 安全性の主たる担保は bubblewrap による隔離。隔離未成立なら自律書込を不許可。
- タイムアウト/キャンセルはプロセスグループ単位 kill＋隔離破棄を検討（子孫残留対策）。
