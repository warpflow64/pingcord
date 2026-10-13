# pingcord

MGMT 機器 (スイッチ、AP、WLC など) の死活を監視し、落ちたら Discord Webhook に通知する小さな監視ツールです。
VyOS の `container` 機能で動かすことを前提に、静的バイナリ 1 つ (イメージ約 4MB) で動きます。

- **blackbox 的な監視**: ICMP (ping) / TCP ポート / HTTP(S) を定期チェック
- **deadman 的な監視**
  - 受信: 機器やジョブから `/ping/<name>` が一定時間届かなければ通知
  - 送信: healthchecks.io などへハートビートを送り、VyOS ごと落ちたことを外部で検知
  - 定期レポート: 毎日決まった時刻に全体の状態を Discord に投稿 (届かなければ pingcord が死んでいる)
- 連続失敗回数での判定 (フラッピング抑制)、ダウン継続中の再通知、復旧通知 (ダウン時間つき)
- 同時に落ちた機器を 1 メッセージにまとめて送信 (上流スイッチ断で配下の AP が一斉に落ちたとき用)
- Discord のレート制限 (429) や回線断時は最大 15 分再送

## 通知の例

| 種類 | 内容 |
|---|---|
| 🔴 DOWN | 種別・宛先・検知開始時刻・エラー内容 (設定したロールをメンション) |
| 🟠 STILL DOWN | `repeat_interval` ごとにダウン継続時間を再通知 |
| 🟢 RECOVERED | ダウン時間・応答時間 |
| ℹ️ / ⚠️ | pingcord の起動・停止 |
| 📊 定期レポート | UP / DOWN 件数とダウン中の一覧 |

時刻は Discord のタイムスタンプ記法なので、見る人のローカル時刻と「5 分前」のような相対表示になります。

## 判定の仕組み

- `interval` ごとにチェックし、`fail_threshold` 回連続で失敗したら DOWN (例: 30s × 3 で約 1.5 分で検知)
- DOWN 中に `recover_threshold` 回連続で成功したら RECOVERED
- 起動時にすでに落ちている対象も、`fail_threshold` 回失敗した時点で DOWN 通知します (生きている対象は通知しません)
- ICMP は 1 回のチェックで `icmp_count` 個送り、1 つでも返れば成功

## VyOS への導入

VyOS 1.4 (sagitta) 以降を想定しています。

### 1. イメージを用意する

**A. GHCR から取得する (おすすめ)**

`main` への push で GitHub Actions が amd64 / arm64 のイメージを `ghcr.io/warpflow64/pingcord` に公開しています。VyOS の op モードで:

```
add container image ghcr.io/warpflow64/pingcord:latest
```

`v1.2.3` のようなタグを push すると `:1.2.3` のタグも作られるので、本番ではバージョンを固定するのがおすすめです。

**B. 手元でビルドして持ち込む (VyOS がインターネットに出られない場合)**

```sh
./scripts/build-image.sh                        # x86_64 の VyOS 用 -> pingcord-amd64.tar
PLATFORM=linux/arm64 ./scripts/build-image.sh   # ARM の場合
scp pingcord-amd64.tar vyos@<router>:/tmp/
```

VyOS 上で:

```sh
sudo podman load -i /tmp/pingcord-amd64.tar      # localhost/pingcord:latest として登録される
```

### 2. 設定ファイルを置く

`/config` 配下は再起動後も残り、設定のバックアップにも含まれます。

```sh
mkdir -p /config/containers/pingcord
vi /config/containers/pingcord/config.yaml   # config.example.yaml を元に作成
```

### 3. コンテナを設定する

```
configure
set container name pingcord image 'ghcr.io/warpflow64/pingcord:latest'
set container name pingcord allow-host-networks
set container name pingcord capability 'net-raw'
set container name pingcord environment TZ value 'Asia/Tokyo'
set container name pingcord environment DISCORD_WEBHOOK_URL value 'https://discord.com/api/webhooks/xxxx/yyyy'
set container name pingcord volume config source '/config/containers/pingcord/config.yaml'
set container name pingcord volume config destination '/etc/pingcord/config.yaml'
set container name pingcord memory '64'
set container name pingcord restart 'always'
commit
save
```

- `allow-host-networks`: VyOS のルーティングをそのまま使って MGMT セグメントへ到達するため
- `capability net-raw`: ping (raw ICMP ソケット) に必要。付け忘れると起動時にエラーで終了します
- 手元からイメージを持ち込んだ場合は `image` を `localhost/pingcord:latest` にしてください

Webhook URL は `config.yaml` に直接書いても構いません (環境変数が空のときは `DISCORD_WEBHOOK_URL` を参照します)。
どちらに書いても VyOS の設定バックアップに含まれる点に注意してください。

### 4. 動作確認

```sh
sudo podman exec pingcord /pingcord -check   # 設定の検証
sudo podman exec pingcord /pingcord -test    # Discord にテスト通知 (メンションも確認できます)
sudo podman logs -f pingcord               # ログ
show container
```

### 設定変更・更新

```sh
vi /config/containers/pingcord/config.yaml
restart container pingcord
```

イメージを更新するときは、GHCR なら `add container image ghcr.io/warpflow64/pingcord:latest` を再度実行し、tar なら `podman load` してから、`restart container pingcord` します。

## 設定

`config.example.yaml` にすべての項目をコメント付きで載せています。主な項目:

| 項目 | 既定値 | 説明 |
|---|---|---|
| `defaults.interval` | `30s` | チェック間隔 |
| `defaults.timeout` | `2s` | 1 回のチェックのタイムアウト |
| `defaults.fail_threshold` | `3` | 連続失敗回数で DOWN |
| `defaults.recover_threshold` | `2` | 連続成功回数で RECOVERED |
| `defaults.repeat_interval` | `0s` (無効) | DOWN 継続中の再通知間隔 |
| `notify.group_wait` | `10s` | この時間内のイベントを 1 メッセージにまとめる (`0s` で即時) |
| `notify.summary_at` | なし | 定期レポートの時刻 (`["09:00", "18:00"]` など) |
| `discord.mention` | なし | DOWN 時のメンション。対象ごとに `mention` で上書き、`none` で無効化 |

ターゲットは `interval` などを個別に上書きできます。

```yaml
targets:
  - name: core-sw01          # ICMP (type 省略時)
    host: 10.255.0.2
  - name: wlc-ssh            # TCP 接続
    type: tcp
    host: 10.255.0.20:22
  - name: controller-web     # HTTP(S)。既定は 2xx/3xx で成功、expect_status で固定も可
    type: http
    url: https://10.255.0.21:8443/
    insecure_skip_verify: true
```

環境変数: `TZ` (定期レポートの時刻)、`LOG_LEVEL` (`debug` / `info` / `warn`)、`LOG_FORMAT=json`。

## deadman (ping 受信型)

`http.listen` を設定すると HTTP サーバーが立ち、以下を提供します。

| パス | 説明 |
|---|---|
| `GET/POST /ping/<name>` | deadman チェックの生存通知 |
| `GET /status` | 全対象の状態 (JSON) |
| `GET /healthz` | pingcord 自体のヘルスチェック (認証なし) |

```yaml
http:
  listen: "10.255.0.1:9095"   # MGMT 側の VyOS のアドレスなど
  token: ${PINGCORD_TOKEN}
deadman:
  - name: config-backup
    timeout: 26h
```

```sh
# バックアップジョブの最後に
curl -fsS -m 10 "http://10.255.0.1:9095/ping/config-backup?token=$PINGCORD_TOKEN"
```

`allow-host-networks` なので VyOS 上のポートを直接使います。MGMT 以外から届かないよう `listen` のアドレスと VyOS の firewall (input) で制限してください。

## VyOS ごと落ちた場合に備えて

pingcord は VyOS 上で動くため、VyOS 自体や上流回線が落ちると Discord に通知できません。次の 2 つで補ってください。

1. **外部ハートビート**: `heartbeat.url` に healthchecks.io や Uptime Kuma (Push モニター) の URL を設定し、
   そちら側で「途絶えたら通知」を Discord に向けて設定する
2. **定期レポート**: `notify.summary_at` を設定し、毎日届いていることを確認する

## 開発

```sh
go test ./...
go run . -config config.example.yaml -check
```

macOS などで root なしで動かすと unprivileged ICMP にフォールバックします。
