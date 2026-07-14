# aw-manager 設計ドキュメント

## 1. What — これは何か

aw-manager は、チャットプラットフォーム（Slack / Discord）から [aw](https://github.com/konono/aw) が管理する AI エージェント Pod を K8s 上で操作するためのゲートウェイサーバー。

ユーザーが Discord/Slack でメッセージを送ると:
1. そのユーザー+チャンネルに対応する K8s Pod を自動作成（または既存を再利用）
2. Pod 内の AI ツール（Claude Code 等）にメッセージを stdin で渡して実行
3. 実行結果をチャットに返信

```
Chat (Discord/Slack) → aw-manager → K8s Pod (claude/codex/opencode/cursor)
```

## 2. Why — なぜ作ったか

aw はローカルのコンテナランタイム（Podman/Docker）で AI エージェントを起動する CLI ツール。
しかし以下のユースケースに対応できなかった:

- **チーム共有**: 複数メンバーが Slack/Discord から AI エージェントを呼びたい
- **常時稼働**: K8s 上に常駐し、メッセージに即応したい
- **ローカル環境不要**: 開発環境がなくてもチャットからエージェントを使いたい

aw-manager はこれらを解決する。aw の既存資産（`aw manifest` によるマニフェスト生成、プロファイルシステム、コンテナイメージ）をそのまま活用し、チャットインターフェースを追加する。

## 3. アーキテクチャ

### 3.1 コンポーネント構成

```
aw-manager (K8s Deployment in aw-system namespace)
├── Chat Adapter (Discord or Slack, Socket Mode)
├── Chat Handler (ビジネスロジック)
├── Pod Manager (K8s client-go + aw manifest subprocess)
├── Session Store (Redis)
├── Metrics Server (Prometheus /metrics)
└── Idle Cleanup (background goroutine)

Agent Pods (K8s Deployments in aw namespace)
├── Created by: aw manifest → kubectl apply (server-side apply)
├── Executed by: kubectl exec (SPDY) with stdin
└── Cleaned up by: idle timeout or pod unhealthy detection
```

### 3.2 Chat Adapter パターン — Why this design

**課題**: Slack と Discord をサポートし、将来的に他のプラットフォーム（Teams 等）も追加できるようにしたい。

**選択**: `chat.Adapter` インターフェースによる Strategy パターン。

```go
type Adapter interface {
    Name() string
    Run(ctx context.Context, handler MessageHandler) error
}
```

**Why not プラットフォーム固有のコードを handler に直書き**:
- プラットフォーム追加のたびに handler が肥大化する
- テスト時に mock adapter を差し込めない
- 各プラットフォームの接続ライフサイクル（Socket Mode, WebSocket）が異なり、一つの Run ループに混ぜると複雑化する

**Why not 共通メッセージバス（NATS 等）を間に挟む**:
- 現時点では 2 プラットフォームのみで over-engineering
- 外部依存を増やしたくない（Redis のみに留めたい）

### 3.3 セッション粒度: ユーザー × チャンネル — Why this granularity

**課題**: 同一ユーザーが複数の会話を並行する場合にセッションが混線する。

**選択**: `SessionKey = {UserID, ChannelID}` — チャンネル単位で Pod を分離。

**検討した代替案**:

| 粒度 | 長所 | 短所 | 採否 |
|---|---|---|---|
| ユーザー単位 | Pod 数が少ない | 複数チャンネルで会話が混線。`--continue` が最新セッションしか再開できずツール非依存にできない | ✗ |
| スレッド単位 | 完全分離 | Pod 数が爆発、スレッドの定義がプラットフォーム間で異なる | ✗ |
| チャンネル単位 | 適度な分離、Pod 数が現実的 | 同一チャンネル内の別トピックは分離できない | ✓ |

**Why not スレッド単位**: 
- Discord の「スレッド」と Slack の「スレッド」は概念が異なる（Discord は channel object、Slack は thread_ts）
- Pod 数がアクティブスレッド数分になり、リソース消費が大きい
- チャンネル単位なら `--continue` がツール非依存で動く（Pod 内に 1 セッションのみ）

### 3.4 aw manifest サブプロセス統合 — Why subprocess, not library import

**課題**: agent Pod の K8s マニフェストを生成する方法。

**選択**: `aw manifest <profile> --name <hash>` をサブプロセスとして呼び、出力 YAML を dynamic client で apply。

**Why not aw の internal パッケージを直接 import**:
- aw は CLI ツールとして設計されており、`profile.Load()` がファイルシステムの `.aw.yml` を探索する前提
- import すると aw の全依存（sqlite, fzf, tcell 等の TUI ライブラリ）が aw-manager に入る
- aw のバージョンアップ時に import パスが壊れるリスク

**Why not aw-manager 内で manifest 生成を再実装**:
- aw manifest の生成ロジック（ConfigMap, Secret, Deployment, security context, init container 等）が複雑で、再実装はメンテナンス負担が大きい
- aw 側の変更に追従する必要がない

**トレードオフ**: サブプロセス実行のオーバーヘッド（~数秒）があるが、Pod 作成は 1 チャンネルにつき 1 回なので許容範囲。

### 3.5 Server-Side Apply — Why SSA

K8s への manifest 適用に Server-Side Apply (SSA) を使用。

**Why not `kubectl apply` サブプロセス**:
- distroless/slim イメージに kubectl を含めたくない
- サブプロセス起動のオーバーヘッド
- エラーハンドリングが困難

**Why not client-go の typed client (Create/Update)**:
- `aw manifest` の出力は多種のリソース（Namespace, SA, ConfigMap, Secret, Deployment）を含む
- typed client では各リソースタイプごとに個別のコードが必要
- SSA なら `dynamic.Interface` + `unstructured.Unstructured` で全リソースを統一的に扱える

### 3.6 ツール非依存の exec — Why stdin, not argv

**課題**: Claude Code, Codex, OpenCode, Cursor の各ツールでメッセージを渡す方法。

**選択**: メッセージを stdin 経由で渡す。

```go
func (m *Manager) ExecTool(ctx, podName, ns string, command []string, message string) (string, error) {
    return m.execWithStdin(ctx, podName, ns, command, strings.NewReader(message))
}
```

**Why not argv (コマンドライン引数)**:
- メッセージが長い場合、argv の長さ制限に当たる
- 特殊文字のエスケープ問題（`"`, `$`, バッククォート等）
- 初期実装では argv で渡していたが、実運用で問題が発生したため stdin に移行

**`ToolCommander` インターフェースで各ツールのコマンドを抽象化**:

```go
type ToolCommander interface {
    PromptCommand(continueSession bool) []string
}
```

各ツールの差異（`--permission-mode bypassPermissions` vs `-a never` 等）をここに閉じ込める。

### 3.7 deploy サブコマンド — Why self-deploying

**課題**: aw-manager 自体を K8s にデプロイするのに kubectl + YAML ファイルを手動で適用するのは面倒。

**選択**: `aw-manager deploy` コマンドで自分自身 + Redis + RBAC を一括デプロイ。

```bash
aw-manager deploy \
  --adapter discord --discord-token xxx \
  --image ghcr.io/konono/aw-manager:0.1.0 \
  --aw-config .aw.yml
```

**deploy が行うこと**:
1. Namespace 作成 (aw-system, aw)
2. ServiceAccount + Role + RoleBinding
3. Redis Deployment + Service（`--redis-url` 未指定時）
4. `.aw.yml` から secrets を自動抽出（env vars + file secrets）
5. Chat トークンを K8s Secret として作成
6. aw-manager Deployment（probes, volumes, env 設定済み）

**`.aw.yml` からの secrets 自動抽出**: `.aw.yml` の `kubernetes.secrets.env` と `kubernetes.secrets.files` を読み、ホストの環境変数とファイルを自動的に K8s Secret/Volume にマッピング。`KEY=VALUE` 形式の inline 値もサポート（aw v4.5.0 で追加）。

### 3.8 Dockerfile — Why debian-slim with agent user

**選択**: `debian:bookworm-slim` + `agent` ユーザー (uid 1001, gid 0)

**Why not distroless**:
- aw-manager は `aw manifest` をサブプロセスで実行するため、shell が必要
- `aw` バイナリが `git` コマンドを内部で使うケースがある

**Why agent ユーザー + `chmod g=u`**:
- aw のコンテナイメージと同じパターン
- OpenShift が arbitrary UID（1000980000 等）で実行するため、group 0 に read/write 権限を付与
- 初期実装では `--env HOME=/tmp` のハックで回避していたが、aw のパターンに合わせて解消

## 4. 既知の制限事項

| 制限 | 理由 | 将来の対応 |
|---|---|---|
| `aw manifest` が Pod 内で実行され、ホストの env/files がない | deploy 時に `.aw.yml` の secrets を自動抽出して Pod にマウントすることで回避 | aw 側で manifest の secrets を外部から注入できる仕組みを追加 |
| tool-config ConfigMap が etcd サイズ制限を超える場合がある | ホストの `.claude/` 配下が大きい場合。Apply 時にスキップする | ConfigMap の内容をフィルタリング |
| `hostUsers` フィールドが一部 K8s ディストリビューションで非対応 | OpenShift の API スキーマが未対応。Apply 時にフィールドを除去 | aw 側で OpenShift 互換の manifest を生成 |
| Redis の RDB スナップショットエラーが発生する場合がある | K8s Pod のファイルシステム制約 | Redis の `save` 設定を無効化、または PVC を使用 |
| codex / opencode は `--continue` を未サポート | ツール側の制限。PromptCommand で continueSession を無視 | ツール側の対応を待つ |
| **replicas: 1 必須** | Slack/Discord の Socket Mode は各インスタンスが独立にイベントを受信する。replicas > 1 だと同一メッセージが複数インスタンスで処理され、二重応答・二重 exec が発生する | リーダー選出、または Socket Mode の代わりに Webhook + 外部ロードバランサー |

## 4.1 運用ガイド

### Redis

- deploy コマンドの組み込み Redis は **volume なし**。Pod 再起動でセッション情報が消える（agent Pod 自体は K8s に残るため、次回メッセージで再検出される）
- **本番では外部 Redis を推奨**: `--redis-url redis://your-redis:6379`
- RDB スナップショットエラーが出る場合は `save ""` で無効化するか PVC を使用

### ネットワーク

- `/metrics` と `/healthz` は認証なしで公開される。**NetworkPolicy でクラスタ内からのアクセスに制限することを推奨**
- チャットでメンション/DM できるユーザーなら誰でも agent Pod を作成できる。**Slack/Discord 側のチャンネル権限で制御する**

### スケーリング

- **aw-manager は replicas: 1 でのみ正しく動作する**。HPA やスケールアウトは行わないこと
- agent Pod 数はアクティブなユーザー×チャンネル数に比例する。idle timeout（デフォルト 1h）で自動回収される

### Pod 回収の仕組み

2 段階の回収メカニズムで orphan Pod を防ぐ:

1. **Idle cleanup** — Redis のセッションキーを走査し、lastActive が idle timeout を超えた Pod を削除（5 分間隔、LockKey で handler と排他制御）
2. **Orphan cleanup** — K8s の `managed-by=aw` ラベル付き Deployment を走査し、Redis にセッションがなく idle timeout を超えたものを削除。4段階の安全装置: (a) Redis 障害時は全体スキップ、(b) creation timestamp が idle timeout 以内なら保護、(c) 削除直前に Redis を再確認、(d) 非 terminal Pod（Pending/Running/ContainerCreating）が存在すればスキップ。K8s API 障害時も安全側に倒す（スキップ）。instance 名→SessionKey の逆引きは不可（ハッシュが不可逆）なので LockKey は使わないが、(a)-(d) で実質的な競合窓は極めて小さい
3. **PodsActive メトリクス** — 起動時 + 各 cleanup ループ後 + Pod 作成/削除後に K8s API から `Set()` で同期。Inc/Dec ではなく同期ベースなのでドリフトしない

## 5. コードマップ

```
aw-manager/
├── main.go                        # kong CLI エントリポイント + .env ローダー
├── internal/
│   ├── cmd/
│   │   ├── cli.go                 # CLI 構造体（serve, deploy, build）
│   │   ├── serve.go               # サーバー起動
│   │   ├── deploy.go              # K8s デプロイ + .aw.yml secrets 自動抽出
│   │   └── build.go               # コンテナイメージビルド
│   ├── chat/
│   │   ├── adapter.go             # Adapter/Responder インターフェース + SplitMessage
│   │   ├── handler.go             # メッセージ処理（Pod確保→exec→返信）
│   │   ├── tool.go                # ToolCommander（claude/codex/opencode/cursor）
│   │   ├── slack/adapter.go       # Slack Socket Mode 実装
│   │   └── discord/adapter.go     # Discord WebSocket 実装
│   ├── pod/manager.go             # Pod ライフサイクル（aw manifest→apply→exec→cleanup）
│   ├── session/store.go           # Redis セッション（user+channel → pod マッピング）
│   ├── manifest/apply.go          # マルチドキュメント YAML パース + SSA
│   ├── k8s/config.go              # K8s REST config 構築（InCluster / kubeconfig）
│   ├── config/config.go           # Config 構造体 + Validate
│   ├── metrics/metrics.go         # Prometheus メトリクス定義
│   ├── server/server.go           # サーバーオーケストレーション
│   └── version/version.go         # バージョン定数（release-please 管理）
├── deploy/                        # 参考用の静的 K8s マニフェスト
├── Dockerfile                     # マルチステージビルド（debian-slim + agent user）
├── .env.example                   # 設定可能な環境変数一覧
├── .goreleaser.yml                # GoReleaser 設定
├── .github/workflows/             # CI + リリースパイプライン
└── docs/
    └── design.md                  # 本ドキュメント
```

## 6. aw との関係

| aw が提供するもの | aw-manager が利用する方法 |
|---|---|
| `aw manifest <profile> --name <instance>` | サブプロセスで呼び、stdout の YAML を apply |
| `.aw.yml` プロファイル設定 | deploy 時に secrets を自動抽出、serve 時に `aw manifest` の入力 |
| コンテナイメージ (`ghcr.io/konono/aw-claude:*`) | agent Pod のイメージとして使用 |
| `kubernetes.secrets.env` の `KEY=VALUE` 形式 | aw v4.5.0 で追加。deploy の自動抽出で活用 |

aw-manager は aw に依存するが、aw は aw-manager を知らない（一方向依存）。
