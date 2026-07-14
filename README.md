# aw-manager

[aw](https://github.com/konono/aw) のチャットゲートウェイ — Slack や Discord から Kubernetes 上の AI コーディングエージェントを操作します。

```
Discord/Slack  →  aw-manager  →  K8s Pod (claude/codex/opencode/cursor)
```

ユーザーがチャットでメッセージを送ると、aw-manager がユーザー+チャンネルごとにエージェント Pod を作成し、AI ツールを実行して結果を返信します。

## クイックスタート

### 前提条件

- Go 1.25+
- Kubernetes クラスタ（OpenShift 対応）
- Redis
- Discord または Slack の Bot トークン
- [aw](https://github.com/konono/aw) バイナリ（`aw manifest` 用）
- K8s プロファイルを含む `.aw.yml`（例: `k8s-claude`）

### ローカル開発

```bash
# 1. 設定ファイルをコピーして編集
cp .env.example .env
vi .env

# 2. Redis を起動
podman run -d --name aw-redis -p 6379:6379 redis:7-alpine

# 3. 実行
./aw-manager serve
```

### Kubernetes へのデプロイ

```bash
# イメージをビルドして push
./aw-manager build --push --registry ghcr.io/yourorg

# デプロイ（secrets は .aw.yml から自動抽出）
./aw-manager deploy \
  --adapter discord \
  --discord-token "your-token" \
  --image ghcr.io/yourorg/aw-manager:0.1.0 \
  --aw-config .aw.yml
```

このコマンド1つで Namespace、RBAC、Redis、Secret、aw-manager Deployment がすべて作成されます。

## 設定

すべての設定は CLI フラグ、環境変数、`.env` ファイルのいずれかで指定できます。`aw-manager serve --help` で全項目を確認できます。

| 設定 | 環境変数 | デフォルト | 説明 |
|---|---|---|---|
| `--adapter` | `CHAT_ADAPTER` | `slack` | チャットプラットフォーム（`slack` or `discord`） |
| `--slack-bot-token` | `SLACK_BOT_TOKEN` | | Slack Bot トークン |
| `--slack-app-token` | `SLACK_APP_TOKEN` | | Slack App トークン（Socket Mode） |
| `--discord-token` | `DISCORD_TOKEN` | | Discord Bot トークン |
| `--redis-url` | `REDIS_URL` | `redis://localhost:6379` | Redis 接続 URL |
| `--aw-profile` | `AW_PROFILE` | `claude-k8s` | エージェント Pod の aw プロファイル |
| `--aw-namespace` | `AW_NAMESPACE` | `aw` | エージェント Pod の Namespace |
| `--aw-binary` | `AW_BINARY` | `aw` | aw バイナリのパス |
| `--aw-tool` | `AW_TOOL` | `claude` | AI ツール（`claude`, `codex`, `opencode`, `cursor`） |
| `--idle-timeout` | `IDLE_TIMEOUT` | `1h` | エージェント Pod のアイドルタイムアウト |
| `--max-concurrent` | `MAX_CONCURRENT` | `10` | 同時メッセージ処理の上限 |
| `--metrics-addr` | `METRICS_ADDR` | `:9090` | Prometheus メトリクスエンドポイント |

## アーキテクチャ

### セッションモデル

**ユーザー + チャンネル** の組み合わせごとに専用の Pod が割り当てられます。同じチャンネル内のメッセージは同じ Pod を再利用し、`--continue` で AI セッションを継続します。異なるチャンネルでは別の Pod が作成され、セッションは分離されます。

### 動作フロー

1. Socket Mode（Slack）または WebSocket（Discord）でチャットメッセージを受信
2. `aw manifest <profile> --name <hash>` で K8s マニフェスト（Deployment, ConfigMap, Secret 等）を生成
3. Server-Side Apply（client-go dynamic client）でマニフェストを適用
4. Pod が Ready になるまで待機
5. `kubectl exec` 相当でメッセージを stdin 経由で AI ツールに渡して実行
6. 応答をチャットに返信

### Pod ライフサイクル

- **作成**: ユーザー+チャンネルの初回メッセージ時
- **再利用**: 同一チャンネルの後続メッセージで既存 Pod を再利用
- **アイドル回収**: `--idle-timeout` を超えてアイドル状態の Pod を自動削除（5分間隔のチェック）
- **Orphan 回収**: Redis にセッションがない Pod（Redis 再起動後など）を安全装置付きでクリーンアップ
- **異常回復**: CrashLoopBackOff / ImagePullBackOff の Pod は削除され、次回メッセージで再作成

### 並行制御

- セッション単位の mutex で EnsurePod + ExecTool を直列化（同一ユーザー+チャンネル）
- グローバル semaphore で同時ハンドラ数を制限（デフォルト: 10）
- 上限超過時はブロックせず即座に「サーバーが混雑しています」と返信

## 対応ツール

| ツール | バイナリ | セッション継続 |
|---|---|---|
| Claude Code | `claude` | `--continue` 対応 |
| Cursor | `agent` | `--continue` 対応 |
| Codex | `codex` | 非対応 |
| OpenCode | `opencode` | 非対応 |

## 可観測性

### Prometheus メトリクス

| メトリクス | 型 | 説明 |
|---|---|---|
| `aw_agent_pods_active` | Gauge | 現在アクティブなエージェント Pod 数（K8s から同期） |
| `aw_agent_exec_duration_seconds` | Histogram | exec 実行時間 |
| `aw_agent_exec_total` | Counter | exec 実行回数（`success`/`error` ラベル） |
| `aw_agent_pod_create_duration_seconds` | Histogram | Pod 作成〜Ready の時間 |
| `aw_agent_messages_total` | Counter | 受信メッセージ数（adapter ラベル） |
| `aw_agent_messages_rejected_total` | Counter | 並行上限による拒否数 |

### ヘルスチェック

`GET /healthz` — Redis 接続可能なら `200 ok`、不可なら `503 unavailable` を返します。

## 本番運用の注意事項

- **単一レプリカ必須** — aw-manager は `replicas: 1` でのみ正しく動作します。Socket Mode は全インスタンスにイベントを配信するため、複数レプリカでは二重処理が発生します。
- **外部 Redis 推奨** — 組み込み Redis は永続化なしです。本番では `--redis-url` で外部の Redis を指定してください。
- **ネットワークポリシー** — `/metrics` と `/healthz` は認証なしで公開されます。NetworkPolicy でアクセスを制限してください。
- **アクセス制御** — Bot にメンションできるユーザーなら誰でもエージェント Pod を作成できます。Slack/Discord のチャンネル権限で制御してください。

## コマンド

```
aw-manager serve    サーバーを起動（デフォルト）
aw-manager deploy   Kubernetes にデプロイ
aw-manager build    コンテナイメージをローカルビルド
```

## ライセンス

MIT
