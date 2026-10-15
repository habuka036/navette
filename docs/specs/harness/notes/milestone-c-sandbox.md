# NAVETTE Milestone C — bubblewrap + Kiro CLI 実機調査

- Date: 2026-10-14
- Environment: DevContainer, `kiro-cli 2.21.1`, bubblewrap (`bwrap`), user `developer` (uid 1000)
- 実験は使い捨て一時ディレクトリ（`/tmp/navc-*`, `/tmp/navrepo-*`）のみ。秘密情報・実
  リポジトリには未接触。認証情報の値は読み取らず、環境変数も名前のみ確認。

Confirmed（確認できた事実）/ Unconfirmed（未確認）を区別する。

## 重大な前提（Confirmed）: Kiro CLI はクラウドエージェント

- Kiro CLI は実行時に **バックエンド（ACP サーバ / LLM）へのネットワーク接続を必須**とする。
  `--unshare-net`（ネットワーク遮断）で実行すると `runStarted` の直後に
  `runError{stage:"init", message:"server shut down unexpectedly"}` ＋ `ACP new_session failed`
  で失敗する。
- Kiro CLI は自身の設定・認証・ランタイム状態として **`~/.local/share/kiro-cli`（書込必須：
  `data.sqlite3` / `run/` ソケット / `.refresh.lock` 等）** と **`~/.kiro`** を必要とする。
  これらを read-only にすると ACP サーバ初期化に失敗する。`/tmp`（`KIRO_TUI_READY_FILE` の
  `/tmp/kiro-tui-ready-*`）も必要。

→ **結論**：Kiro CLI を Runtime とする限り、ネットワーク完全遮断・認証完全 air-gap の
  サンドボックスは成立しない。これは Kiro CLI がクラウドエージェントである以上、不可避。
  NAVETTE が提供できる隔離は「ファイルシステム書込の作業コピー限定」「参照先 read-only」
  「ホスト SSH Agent / Docker Socket / `~/.ssh` 非露出」であり、**ネットワーク到達性と
  Kiro 自身の資格情報ディレクトリは必要上開く**。この制約を運用者へ明示する。

## 成立した最小隔離構成（Confirmed, TEST C/D）

bubblewrap 引数（要点）：

```
bwrap \
  --ro-bind /usr /usr --ro-bind /lib /lib --ro-bind /bin /bin --ro-bind /etc /etc \
  --ro-bind <kiro-cli bin dir> <same>            # 実行バイナリ（読み取り専用）
  --ro-bind <mise dir> <same>                      # 必要ツールチェーン（読み取り専用）
  --bind ~/.local/share/kiro-cli <same>            # Kiro ランタイム状態（書込必須）
  --bind ~/.kiro <same>                            # Kiro 設定（書込必須）
  --ro-bind <各参照リポジトリ> /refs/<name>         # 参照は読み取り専用（Imagorium 等）
  --proc /proc --dev /dev --bind /tmp /tmp \
  --bind <使い捨て作業コピー> /work --chdir /work   # 唯一の書込可能作業領域（.git 除外）
  --setenv HOME /home/developer --setenv PATH ... \
  --share-net                                       # ネットワーク必須（Kiro クラウド）
  kiro-cli chat <prompt> --no-interactive --output-format stream-json --agent-engine <v2|v3> --trust-tools=
```

検証した安全性（TEST D, Confirmed）：
- `/work`（使い捨て作業コピー）のみ書込可。
- 参照リポジトリ（`--ro-bind`）と その `.git` は書込拒否（`Read-only file system`）。
- `/run/docker.sock`・SSH Agent・`~/.ssh` はサンドボックス内に非存在（非露出）。
- 実験後、ホスト側参照リポジトリの内容は不変。

## stream-json 再確認（Confirmed, Milestone A 結果を再利用し一致確認）

- イベント: `runStarted` / `metadata` / `sessionUpdate`(agent_message_chunk / tool_call /
  tool_call_update) / `runFinished`(status/stopReason/finalText) / `runError`(stage/message)。
- 成否は **`runFinished.status`（"success" 等）と `runError` の有無**で判定する。
  プロセス終了コード 0 だけで成功としない（無効 model でも exit 0 の既知事例あり）。
- `--agent-engine v2` 必須（v1 不可）。v3 も可（`--v3` 起動フラグとは別物）。

## 設計上の判断（本 Milestone C 最小スライスの方針）

1. **ネットワークはデフォルト共有**（Kiro CLI 必須）。ただし bubblewrap 隔離で**書込は作業
   コピーのみ**に限定し、参照は read-only、ホスト秘密資源は非露出とする。この「ネット開放・
   FS 隔離」構成を既定とし、運用者へ制約を明示する。
2. **Kiro の資格情報ディレクトリ（`~/.local/share/kiro-cli`, `~/.kiro`）は書込マウントが必要**。
   これは Kiro 自身の認証・状態であり、NAVETTE が新たに秘密を注入するものではない。SSH 鍵・
   Docker Socket・その他ホスト秘密は引き続き非露出。
3. **fail-closed**：bubblewrap が利用不可、または作業コピー準備（`.git` 除外コピー）に失敗した
   場合は、実リポジトリに対する実行を行わず停止する。

## Unconfirmed / 要再検証（Milestone C 残課題）

- `--unshare-net` 下でローカル Provider（将来の Ollama 等）だけに限定開放する方式（Kiro CLI は
  不可だが、別 Runtime 向けのネットワークポリシーは別途）。
- Kiro CLI がバックエンドへ送信する作業コピー内容の範囲（データ送出のガバナンス）は未調査。
  秘密を含まない使い捨てプロジェクトのみで実機検証する前提を維持。
- 子孫プロセス（ACP サーバ: bun/node、ツール起動プロセス）の完全な回収は、プロセスグループ
  kill ＋サンドボックス破棄で対応予定（本スライスで実装・テスト）。



## 追補（2026-10-14）: 専用プロファイル分離と /tmp 隔離の実機確認

レビュー条件（ホスト Kiro プロファイルを直接書込マウントしない／`/tmp` を tmpfs 隔離）に
基づき再検証した。

### /tmp 隔離（Confirmed）
- `--tmpfs /tmp`（サンドボックス専用一時領域）で問題なし。ホスト `/tmp` の bind は不要。
- 書込可能領域は `/work`（作業コピー）、`/tmp`（tmpfs）、および指定した専用 HOME のみに
  限定できる（意図しないホストパスは書込可能にならない）。

### 専用プロファイル分離（Confirmed だが未認証, TEST E）
- 専用 HOME（`--setenv HOME /nhome` ＋ `--bind <tmp dir> /nhome`）で起動すると、Kiro CLI は
  ホストプロファイルに依存せず自前の `~/.kiro` と `~/.local/share/kiro-cli` を**新規作成**する
  （分離は成立）。実行バイナリ本体（`bun`/`node`）のみ read-only で参照すれば足りる。
- しかし専用プロファイルは**未認証**のため、Kiro CLI は認証フロー（ブラウザ/device flow）を
  試み、`kiro-cli login --use-device-flow` を促して失敗する。すなわち専用プロファイルでの
  実行には**明示的な一度きりの人間によるログイン**が必要。

### 認証に関する制約（重要・停止理由）
- `kiro-cli login` はブラウザリダイレクトまたは device flow のいずれも**人間の対話操作**
  （URL アクセス・コード入力）を要する。非対話での資格情報注入手段は（ホスト秘密のコピーを
  除き）存在しない。
- ホスト側の認証はプロファイル状態と同じ `data.sqlite3` に格納されており、トークンのみを
  read-only 参照する分離は困難（状態と資格情報が同一ストア）。
- レビュー制約「ホストの既存資格情報を無断でコピー・表示・ログ記録しない」を厳守するため、
  **専用プロファイルの認証は人間の明示操作が必要**であり、エージェントが自律的に成立させる
  ことはできない。host profile の `whoami` は「GitHub ログイン済み」であることのみ確認
  （メールアドレス等の個人情報・トークン値はログに残さない方針）。

### 代替案（人間の判断が必要）
1. **専用プロファイルを人間が一度 device flow でログイン**して用意し、その専用 HOME を
   NAVETTE が使う（分離を保ちつつ認証成立。推奨だが人間の一手が必要）。
2. ホストプロファイルを **read-only** で参照する妥協案（ただし Kiro は書込を要するため
   そのままでは起動不可。コピーオンライト等の追加設計が必要で、かつ「無断コピーしない」
   制約と緊張する）。
3. Kiro 実接続を保留し、ローカル完結 Runtime を先行（当初方針「初期 Runtime=Kiro CLI」と
   ずれる）。

→ FS 隔離・/tmp 隔離・専用プロファイル分離自体は成立。残る唯一のブロッカーは「専用
  プロファイルの認証に人間の一度きりのログインが必要」である点。安全性を犠牲にせず、
  この判断を仰ぐため停止する。



## 実装結果（Milestone C 最小スライス, 2026-10-14）

認証済み専用プロファイル（Builder ID, `~/.navette/kiro-home`）で実装・実機検証した。

### 実装パッケージ
- `internal/sandbox/`：`sandbox.go`（bwrap コマンド構築：work copy のみ書込・refs は ro-bind・
  `--tmpfs /tmp`・専用 HOME bind・`--unsetenv SSH_AUTH_SOCK`・`--share-net`・`--die-with-parent`・
  fail-closed）、`workcopy.go`（`.git` 除外の使い捨て作業コピー生成/削除）、`diff.go`（作業コピー
  差分の算出・人間向け表示）。
- `internal/runtime/kiroruntime/`：`streamjson.go`（stream-json パーサ、成否は
  `runFinished.status`/`runError` で判定、exit code 非依存、不正行は parse_error として継続）、
  `kiro.go`（sandbox 経由で kiro-cli 起動、`StreamingRuntime` 実装、Setpgid でプロセスグループ
  起動、cancel/timeout 時に SIGTERM→猶予→SIGKILL でグループごと回収）。
- `internal/engine/`：`executeStep` が `StreamingRuntime` を検出し `EventSink`（`sqlSink`）で
  出力イベントを 1 件ずつ SQLite へ逐次記録。永続化失敗時は note を残し以降停止（成功扱いにしない）。
- `cmd/navette`：`run-kiro` サブコマンド。fail-closed（bwrap 無し/未認証で停止）、専用 HOME の
  `whoami` 認証確認（資格情報は読まない）、`.git` 除外作業コピー、結果承認前に差分＋実行ログを
  表示する対話ゲート（自動承認なし）。

### 実機 E2E（秘密なし使い捨て Go プロジェクト, Confirmed）
- 事前承認 → 実 Kiro CLI がサンドボックス内で `main.go` を編集 → `runFinished:success` →
  結果承認で差分 `[modified] main.go` を提示 → 承認 → **Succeeded**。
- **ホスト側ソースは不変**（作業コピーのみ編集、実リポジトリへ自動反映しない）。
- fail-closed 検証：未認証プロファイル→ログイン手順表示で停止（exit 7）、bwrap 不在→実行拒否
  （exit 6）。

### 確認できた安全境界（Confirmed）
- 書込可能はサンドボックス内 `/work`（作業コピー）＋ `/tmp`（tmpfs）＋ 専用 HOME のみ。
- 参照（refs）・ホスト OS は read-only。`.git` は作業コピーから除外。
- Docker Socket / SSH Agent（`SSH_AUTH_SOCK` unset）/ `~/.ssh` 非露出。
- 成否は stream-json ベース（exit code 非依存）。
- cancel/timeout でプロセスグループごと回収（ACP サーバ・ツール子孫を含む）。

### 残存リスク（Milestone C 時点）
- ネットワークは Kiro クラウドへ開放（通信先・送信内容は制限しない）。初期 PoC は秘密を含まない
  使い捨てプロジェクト限定。egress 制御は将来課題。
- 専用 HOME は Kiro の認証情報を含むため、エージェントプロセスから完全秘匿ではない。
- 認証タイプは Builder ID（`--license free` 経由）。普段の GitHub 直ログインとは表示が異なる。
- プロセスグループ回収は実装済みだが、サンドボックス外に逃れる detached プロセスの網羅的検証は
  今後の課題（本スライスでは bwrap 内で完結する前提）。



## レビュー補修（P0/P1, 2026-10-14）

コードレビュー指摘に対する補修を実施。実機 E2E（秘密なし使い捨て Go プロジェクト）で確認済み。

### P0
- **成果物の永続化**：`cmdRunKiro` は作業コピーを無条件削除しない。run 単位の永続ディレクトリ
  （`--artifacts/<runID>`）へ `sandbox.SaveArtifacts` で保存し、`runs` テーブルの
  `artifact_dir`/`artifact_hash` に記録（`store.CreateRun`/`SetRunArtifacts`/`FinishRun`/
  `RunArtifacts`）。却下・中断時も成果物を保持。作業コピーは保存後にのみ削除。実リポジトリへの
  反映・commit・push はしない。
- **イベント保存失敗の伝播**：`runtime.EventSink.Emit` を `error` 返却に変更。`StreamingRuntime`
  は Emit エラーでステップ中断。Engine の `sqlSink` は失敗を記録して返し、`executeStep` が
  エラー化 → runLoop が Failed にし、結果承認/Succeeded へ進まない。SQLite 書込失敗を模す
  `failEventStore` テストで検証。
- **プロセス回収**：`reclaimGroup` を修正。`exited`（`cmd.Wait()` 後 close）で早期終了を判定し、
  猶予満了時は確実に `SIGKILL(-pgid)`。stdout drain＋Wait＋reclaimer の協調を整理。SIGTERM 無視
  子プロセス／正常終了／SIGTERM 尊重の 3 テスト追加。完全回収を保証できない範囲（サンドボックス外へ
  逃れた detached プロセス）は明記。
- **環境変数の隔離**：bubblewrap を `--clearenv` 既定にし、`HOME`/`PATH` と `ExtraEnv`
  allowlist のみ設定。bwrap 親プロセスの `cmd.Env` も最小化。ホスト API キー非継承をテスト。
  専用 Kiro HOME の認証情報露出は残存リスクとして継続明記。
- **結果承認の実効性**：`sandbox.UnifiedDiff` でテキストファイルの unified diff を提示。binary は
  フラグ表示、added/deleted も表現。差分または fingerprint 取得失敗時は結果承認を拒否
  （fail-closed）。承認時の作業コピー fingerprint（`sandbox.Fingerprint`）と保存成果物の
  fingerprint 一致を検証し、不一致なら exit 8。

### P1
- 作業コピー生成（`PrepareWorkCopy`/`PrepareWorkCopyOpts`）で `.git`/`.hg`/`.svn`/`node_modules`/
  `.ssh` 等のディレクトリと `.env`/鍵/`credentials`/`*.pem` 等の秘密ファイルを除外。per-file と
  total のサイズ上限（既定 32MiB/256MiB）で fail-closed。`RemoveWorkCopy` は workcopy 命名を検証。
- stream-json：`runFinished`/`runError` の重複・矛盾を `ErrProtocol` で fail-closed。
- `kiroruntime.Execute` のイベント収集を修正（返却 `RunResult.Events` に確実に反映）。
- diff/fingerprint の snapshot も作業コピーと同じ除外規則を適用し、除外ファイルが差分に偽の
  追加/削除として現れないようにした。

### 新 CLI 挙動
- `run-kiro --artifacts <dir>` で保存先指定（既定 `~/.navette/artifacts`）。exit code：
  成功 0 / run error 1 / awaiting approval 3 / rejected 4 / persistence failure 5 /
  sandbox 不可 6 / 未認証 7 / 承認・保存 fingerprint 不一致 8。

### 実機 E2E（Confirmed）
- 事前承認 → 実 Kiro CLI がサンドボックスで `main.go` 編集 → unified diff（`-Println("hello")`/
  `+Println("hello-navette")`）提示 → 結果承認 → **Succeeded**、成果物を `--artifacts/<runID>` に保存
  （`.env` 等の秘密は成果物にも diff にも非出現）、**ホストソース不変**、承認＝保存の fingerprint 一致。

### 残存リスク（更新）
- ネットワークは Kiro クラウドへ開放（通信先・送信内容は未制限、egress 制御は将来課題）。
- 専用 HOME は Kiro 認証情報を含み、エージェントから完全秘匿ではない。
- プロセス回収はプロセスグループ単位。サンドボックス外へ二重 fork で逃れる detached プロセスの
  網羅的回収は保証範囲外（bwrap 内完結＋`--die-with-parent` を前提）。



## 追加補修（レビュー2巡目, 2026-10-14）

1. **成果物保存失敗で exit 0 にしない**：保存・SQLite 記録・検証を Engine の `ResultFinalizer`
   として結果承認後〜Succeeded 前に実行。いずれか失敗すると Engine は Failed にし、CLI は
   非 0（finalization 失敗 exit 8）。失敗時は作業コピー・保存済み成果物を削除せず保持（回復可能）。
2. **結果承認と成果物確定の順序を再設計**：状態機械に `Finalizing` を追加。
   `AwaitingResultApproval →(承認を原子的に記録)→ Finalizing →(保存+記録+fingerprint 一致検証)→
   Succeeded`。fingerprint 不一致や保存失敗は Finalizing から Failed へ。**Succeeded になった後に
   不一致が判明する状態を排除**。承認記録と状態遷移の原子的確定（`ApproveAndTransition`）は維持。
   承認時に作業コピーの fingerprint を確定し、保存成果物とその同一性を確認してから成功確定。
3. **プロセス回収の再検証**：`cmd.Wait()`（直接の子の reap）とプロセスグループ全体の終了を同一視
   しない。parse エラー・EventSink 保存失敗時にも `reclaimTrigger` でグループを能動的に
   SIGTERM→猶予→SIGKILL。SIGTERM を無視する子孫（PID ファイルで追跡）が確実に停止することを
   テスト（EventSink 失敗／parse エラーの両方）。完全回収の保証範囲（二重 fork で bwrap 外へ逃れる
   プロセスは対象外）は引き続き明記。
4. **RemoveWorkCopy の厳格化**：部分一致を廃止。絶対パス・`navette-workcopy-` で始まる base 名・
   OS 一時ディレクトリ直下・シンボリックリンクでない・ディレクトリであること、を全て検証。
   非該当パス（部分一致のみ／temp 外／相対／空／symlink）は削除拒否することをテスト。

### 失敗注入テスト（追加）
- engine：`TestFinalizerFailure_NeverSucceeds`（finalize 失敗で Succeeded にならず Failed）、
  `TestFinalizer_RunsBeforeSuccess`（finalize は Finalizing 状態で実行＝成功前）。
- cmd/navette：`TestFinalizer_SaveFailureReturnsError`、`TestFinalizer_FingerprintMismatchReturnsError`、
  `TestFinalizer_SuccessVerifies`。
- kiroruntime：`TestEventSinkFailureStopsProcessGroup`、`TestParseErrorStopsProcessGroup`
  （SIGTERM 無視子孫を PID 追跡で確実停止確認）。
- sandbox：`TestRemoveWorkCopy_StrictValidation`、`TestRemoveWorkCopy_RefusesSymlink`。

### 実機 E2E（Confirmed）
`run-kiro`：事前承認 → 実 Kiro CLI で `main.go` 編集 → unified diff 提示 → 結果承認 →
「artifacts saved and verified」→ **Succeeded**。成果物は `--artifacts/<runID>` に保存・SQLite 記録・
承認 fingerprint と一致検証済み。ホストソース不変。成功は成果物確定の後にのみ確定。



## 追加補修（レビュー3巡目, 2026-10-14）

1. **FinishRun エラーを無視しない**：`cmdRunKiro` は `FinishRun` の戻り値を検査し、run 終了記録に
   失敗した場合は exit 0 を返さない（exit 5「run completion persistence failure」）。記録失敗時は
   作業コピーを保持（`safeToRemove` に `finishErr == nil` を条件追加）。成功確定は「成果物確定
   （finalize）」の後、run 終了記録の失敗検査も経てから。
2. **reclaim の保証強化**：`exited`（`cmd.Wait()` 完了）は直接の子の reap を示すだけでグループ全体の
   消滅を保証しないため、`cmd.Wait()` 後に **無条件で `sweepGroup`（プロセスグループへ SIGKILL）** を
   実行し、SIGTERM 無視・detached の孫を含む残存メンバーを確実に reap。テスト追加：
   `TestChildExitsButGrandchildRemains`（直接の子が即終了し SIGTERM 無視の孫が残るケースで、猶予後
   SIGKILL により孫が消えることを PID 追跡で確認）。parse 失敗・EventSink 失敗時の確実停止テストは
   継続。setsid 等でグループを離脱した孫は対象外（bubblewrap `--die-with-parent` が backstop）。
3. **Finalizing 中クラッシュの回復方針**：`RecoverInterrupted` が `Running` に加え **`Finalizing` も
   `Indeterminate` へ**遷移（状態機械に `Finalizing → Indeterminate` を追加）。再起動後に自動で
   Succeeded にせず、未確定（保存途中・未検証）の成果物を保持し人間の確認を要求。理由文に「artifacts
   unverified, human review required」を記録。テスト：`TestRecoverInterrupted_FinalizingToIndeterminate`
   （Indeterminate→Succeeded が不許可であることも確認）。
4. **0 件 UPDATE の検出**：`SetRunArtifacts` / `FinishRun` は `RowsAffected()==0`（存在しない run ID）
   を `ErrNotFound` にする。テスト：`TestSetRunArtifacts_UnknownRunErrors` / `TestFinishRun_UnknownRunErrors` /
   `TestRunArtifacts_RoundTrip`。

### 検証（3巡目）
gofmt クリーン、`go build/vet ./...` OK、全パッケージ test green（task/store/sandbox/kiroruntime/
engine/cmd）。実機 E2E：承認 →「artifacts saved and verified」→ Succeeded、ホスト不変。millefeuille
非回帰 green。exit code は従来に加え run 完了記録失敗も 5。

### 残存リスク（更新）
- プロセス回収はプロセスグループ単位（SIGKILL sweep ＋ cancel 時 SIGTERM→SIGKILL）。`setsid`/二重 fork で
  グループを離脱した孫は process-group シグナルの対象外。bubblewrap の `--die-with-parent` と、将来の
  サンドボックス破棄が backstop。完全回収は保証範囲外として明記。
- ネットワーク開放・専用 HOME の認証情報・egress 無制限は従来どおり残存リスク。
