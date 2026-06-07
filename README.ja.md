# oci2mtk

[English](README.md)

## これはなに？

`docker save` コマンドで保存した `.tar` ファイルを MikroTik RouterOS 向けに変換するツールです。

containerd が有効な環境で出力された OCI 準拠のコンテナイメージや、docker-archive 互換のコンテナイメージに対応しています。

## 使い方

`oci2mtk <IN_FILE> [-d] [-f] [-p <PLATFORM>] [-t <TAG>] [-o <OUT_FILE>]`

- `<IN_FILE>` 入力ファイル名 ― `.tar` のほか `.tar.gz` も受け付ける
- `-d` dry run ― 実際の書き出しをせずに変換処理を実行する
- `-f` 上書き ― `<OUT_FILE>` が既に存在していた場合に上書きする
- `-p <PLATFORM>` 変換の対象とするプラットフォーム (通常は設定不要)
- `-t <TAG>` 変換の対象とするタグ (通常は設定不要)
- `-o <OUT_FILE>` 変換後のファイル名 ― 拡張子 `.tar` または `.tar.gz` を指定できる

### 終了コード

- `0` 正常終了
- `1` 変換不要
- `2` 変換失敗
- `3` 引数エラー

## RouterOS に対応するコンテナイメージの仕様

以下の条件は RouterOS 7.21 で確認したものです。**MikroTik の公式情報ではありません。**

- `manifest.json`, `config.json`, `layer*.tar` が最上位のエントリーになっていること
- `oci-layout`, `index.json` が含まれていないこと
- レイヤーイメージのファイル名は、拡張子が `.tar` になっていること

## 制限事項

- OCI 形式のネストした index には対応していません。
- zstd 圧縮レイヤーには対応していません。
- 1,000 を超えるレイヤー数には対応していません。

## ライセンス

UPL 1.0

## 著者

株式会社さきラボ

