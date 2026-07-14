# Release Workflow トラブルシューティング

aw-manager のリリースパイプライン（release-please + GoReleaser + Docker image publish）で遭遇した問題と解決策の記録。他のリポジトリでも参考にできる。

## アーキテクチャ

```
push to main
  → release-please: Release PR を作成/更新
  → PR マージ時: release-please がタグ + GitHub Release を作成

tag push (v*)
  → goreleaser: Go バイナリをビルドして Release に添付
  → publish-image: Docker イメージをビルドして ghcr.io に push
```

release-please と goreleaser/publish-image は**別トリガー**にしている（後述の Fine-grained token 問題の回避策）。

## 問題1: Fine-grained PAT で `git/refs` / `git/trees` が使えない

### 症状

```
release-please failed: Error creating Pull Request:
Resource not accessible by personal access token
- https://docs.github.com/rest/git/refs#create-a-reference
```

または:

```
release-please failed: Error adding to tree: <sha>
```

### 原因

GitHub の Fine-grained Personal Access Token は `Contents: Read and Write` 権限があっても、Git Data API（`git/refs`, `git/trees`, `git/commits`）の一部操作が `403 Resource not accessible` で拒否される。これは GitHub の既知の制限。

Classic PAT（`repo` スコープ）ではこの問題は発生しない。

### なぜ aw リポでは動くのか

aw リポでは release-please が初回実行時にブランチと PR を作成済み。以降の実行はブランチの**更新**（`git/refs` の update）と PR の**更新**（Contents API）のみで、`git/refs` の **create** や `git/trees` の **create** は不要。

新規リポでは初回に必ずブランチ作成 + tree 作成が必要なため、Fine-grained token の制限に当たる。

### 解決策

#### A. Classic PAT を使う（最も簡単）

https://github.com/settings/tokens/new で `repo` スコープ付きの Classic token を作成し、`RELEASE_PLEASE_TOKEN` に設定する。

```bash
echo "ghp_xxxx" | gh secret set RELEASE_PLEASE_TOKEN --repo owner/repo
```

#### B. GITHUB_TOKEN にフォールバック + タグトリガー分離（aw-manager の方式）

Fine-grained token しか使えない場合の回避策:

1. release-please は `GITHUB_TOKEN` にフォールバック:

```yaml
- uses: googleapis/release-please-action@v4
  with:
    token: ${{ secrets.RELEASE_PLEASE_TOKEN || secrets.GITHUB_TOKEN }}
```

2. `GITHUB_TOKEN` で作成されたリリースは後続ワークフローをトリガーしないため、goreleaser と publish-image は **tag push イベント**で別途トリガー:

```yaml
on:
  push:
    branches: [main]
    tags: ['v*']

jobs:
  release-please:
    if: ${{ !startsWith(github.ref, 'refs/tags/') }}

  goreleaser:
    if: startsWith(github.ref, 'refs/tags/v')

  publish-image:
    if: startsWith(github.ref, 'refs/tags/v')
```

#### C. 初回のみ手動でブランチ + PR を作成

Fine-grained token が `git/refs` の update はできる場合（create のみ不可）:

```bash
# ブランチを手動作成
SHA=$(gh api /repos/owner/repo/git/refs/heads/main --jq '.object.sha')
gh api --method POST /repos/owner/repo/git/refs \
  -f ref="refs/heads/release-please--branches--main" \
  -f sha="$SHA"

# version.go と manifest を更新してコミット
git checkout release-please--branches--main
# ... version bump ...
git commit -m "chore(main): release X.Y.Z"
git push origin release-please--branches--main

# PR を作成
gh pr create --base main --head release-please--branches--main \
  --title "chore(main): release X.Y.Z" --body "..."
```

PR マージ後は release-please が引き継ぐ（ブランチの**更新**のみで動作）。

ただし Fine-grained token では `git/trees` API もブロックされるため、release-please が PR のコミットを更新する際にも失敗する場合がある。その場合は **解決策 D**（手動リリース）に進む。

#### D. 完全手動リリース（Fine-grained token で解決できない場合の最終手段）

release-please の `git/trees` API 制限を完全に回避し、手動でリリースの全工程を行う。

```bash
# 1. release-please ブランチを作成（gh CLI は gho_ トークンで権限あり）
SHA=$(gh api /repos/owner/repo/git/refs/heads/main --jq '.object.sha')
gh api --method POST /repos/owner/repo/git/refs \
  -f ref="refs/heads/release-please--branches--main" \
  -f sha="$SHA"

# 2. ブランチをチェックアウトしてバージョンを更新
git fetch origin release-please--branches--main
git checkout release-please--branches--main
git reset --hard origin/main

# version.go を更新
# x-release-please-start-version の行を新バージョンに変更
vi internal/version/version.go

# .release-please-manifest.json を更新
vi .release-please-manifest.json

# CHANGELOG.md を作成
cat > CHANGELOG.md << 'EOF'
# Changelog

## [X.Y.Z](https://github.com/owner/repo/compare/vA.B.C...vX.Y.Z) (YYYY-MM-DD)

### Features
* feature description (commit hash)

### Bug Fixes
* fix description (commit hash)
EOF

# コミットして push
git add -A
git commit -m "chore(main): release X.Y.Z"
git push --force origin release-please--branches--main

# 3. PR を作成
gh pr create --base main --head release-please--branches--main \
  --title "chore(main): release X.Y.Z" \
  --body "## [X.Y.Z] - Release notes here"

# 4. PR をマージ（GitHub UI またはコマンド）
gh pr merge --squash

# 5. タグと GitHub Release を手動作成
git checkout main && git pull
MERGE_SHA=$(git rev-parse HEAD)
gh api --method POST /repos/owner/repo/git/refs \
  -f ref="refs/tags/vX.Y.Z" \
  -f sha="$MERGE_SHA"

gh release create vX.Y.Z \
  --title "vX.Y.Z" \
  --notes "Release notes here"

# 6. タグ push で goreleaser + publish-image が自動トリガーされる
#    （workflow が tag push をトリガーに設定している場合）
#    トリガーされない場合は、タグを削除して再作成:
gh api --method DELETE /repos/owner/repo/git/refs/tags/vX.Y.Z
git tag -f vX.Y.Z
git push origin vX.Y.Z
```

### トークン権限の検証方法

問題がトークンの権限なのかを特定するために、API を直接テストする:

```bash
# トークンの値を変数に設定
export TEST_TOKEN="github_pat_xxxx"

# git/refs (ブランチ作成) をテスト
SHA=$(curl -s -H "Authorization: token $TEST_TOKEN" \
  https://api.github.com/repos/owner/repo/git/refs/heads/main \
  | grep -o '"sha": "[^"]*"' | head -1 | cut -d'"' -f4)

curl -s -X POST \
  -H "Authorization: token $TEST_TOKEN" \
  -d "{\"ref\":\"refs/heads/test-token\",\"sha\":\"$SHA\"}" \
  https://api.github.com/repos/owner/repo/git/refs

# 成功すれば ref が返る。403 なら権限不足。

# テストブランチを削除
curl -s -X DELETE \
  -H "Authorization: token $TEST_TOKEN" \
  https://api.github.com/repos/owner/repo/git/refs/heads/test-token

# git/trees (ツリー作成) をテスト
curl -s -X POST \
  -H "Authorization: token $TEST_TOKEN" \
  -d "{\"base_tree\":\"$SHA\",\"tree\":[{\"path\":\"test.txt\",\"mode\":\"100644\",\"type\":\"blob\",\"content\":\"test\"}]}" \
  https://api.github.com/repos/owner/repo/git/trees

# 成功すれば tree が返る。403 なら Fine-grained token の制限。
```

両方 403 → Classic PAT が必要。`git/refs` のみ 403 → 手動ブランチ作成（解決策 C）で回避可能。両方 403 → 完全手動リリース（解決策 D）。

## 問題2: `RELEASE_PLEASE_TOKEN` が `Input required and not supplied`

### 症状

```
release-please failed: Input required and not supplied: token
```

### 原因

GitHub Actions の secret `RELEASE_PLEASE_TOKEN` が未設定、または値が空。

### 解決策

```bash
# 値を確認（設定済みなら日時が表示される）
gh secret list --repo owner/repo

# 設定（--body で値を直接指定すると確実）
gh secret set RELEASE_PLEASE_TOKEN --repo owner/repo --body "ghp_xxxx"
```

`echo "token" | gh secret set` は改行が含まれることがある。`--body` の方が確実。

## 問題3: GoReleaser がリリースの重複で失敗

### 症状

```
release already exists
```

### 原因

手動でリリースを作成済みで、GoReleaser が同じタグで再度リリースを作ろうとしている。

### 解決策

`.goreleaser.yml` に `release.mode: keep-existing` を設定:

```yaml
release:
  mode: keep-existing
```

これで既存リリースがあればスキップし、アセット（バイナリ）のみ追加する。

## 問題4: Docker イメージの publish がタイムアウト

### 症状

```
The job has exceeded the maximum execution time of 15m0s
```

### 原因

`--platform linux/amd64,linux/arm64` の multi-arch ビルドは QEMU エミュレーションで遅い。特に Go のコンパイル（aw binary のソースビルド含む）は 15 分を超えることがある。

### 解決策

```yaml
publish-image:
  timeout-minutes: 30  # 15 → 30
```

## 問題5: push したイメージが K8s から pull できない

### 症状

```
ImagePullBackOff
```

### 原因

ghcr.io のコンテナパッケージがデフォルトで **private** になる。

### 解決策

1. https://github.com/users/<user>/packages/container/package/<name> にアクセス
2. Package settings → Danger Zone → Change package visibility → **Public**

または `podman system prune -af` でキャッシュを消した後に再 push した場合、レイヤーが変わって K8s ノードのキャッシュが無効になる。タグを変えて（例: `0.1.0-2`）pull し直す。

## 問題6: Fine-grained token のリポジトリアクセス

### 症状

aw リポで動くトークンが aw-manager で動かない。

### 確認方法

https://github.com/settings/personal-access-tokens で対象トークンの **Repository access** に新しいリポジトリが含まれているか確認。

Fine-grained token は作成時に指定したリポジトリのみにアクセスできる。新しいリポを追加するにはトークンの設定を編集して Repository access に追加する。

## ワークフローのテンプレート

他のリポジトリで使う場合のテンプレート:

```yaml
# .github/workflows/release.yml
name: Release
on:
  push:
    branches: [main]
    tags: ['v*']

permissions:
  contents: write
  pull-requests: write
  packages: write

jobs:
  release-please:
    if: ${{ !startsWith(github.ref, 'refs/tags/') }}
    runs-on: ubuntu-latest
    steps:
      - uses: googleapis/release-please-action@v4
        with:
          token: ${{ secrets.RELEASE_PLEASE_TOKEN || secrets.GITHUB_TOKEN }}

  goreleaser:
    if: startsWith(github.ref, 'refs/tags/v')
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v6
        with:
          go-version: '1.25'
      - uses: goreleaser/goreleaser-action@v7
        with:
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}

  publish-image:
    if: startsWith(github.ref, 'refs/tags/v')
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@v5
      - uses: docker/setup-qemu-action@v3
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Build and push
        run: |
          VERSION=${GITHUB_REF_NAME#v}
          docker buildx build --platform linux/amd64,linux/arm64 \
            -t ghcr.io/${{ github.repository }}:${VERSION} \
            -t ghcr.io/${{ github.repository }}:latest \
            --push .
```

### 必要なファイル

| ファイル | 内容 |
|---|---|
| `release-please-config.json` | `{"release-type": "go", "packages": {".": {"extra-files": [{"type": "generic", "path": "internal/version/version.go"}]}}}` |
| `.release-please-manifest.json` | `{".": "0.1.0"}` |
| `internal/version/version.go` | `x-release-please-start-version` / `x-release-please-end` マーカー付き |
| `.goreleaser.yml` | `release.mode: keep-existing` を推奨 |

### 初回セットアップ手順

1. 上記ファイルを作成してコミット
2. `gh secret set RELEASE_PLEASE_TOKEN --repo owner/repo --body "ghp_xxx"` （Classic PAT 推奨）
3. main に push → release-please が Release PR を作成
4. PR マージ → タグ作成 → goreleaser + publish-image が実行
