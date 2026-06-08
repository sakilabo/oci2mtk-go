# oci2mtk

[日本語](README.ja.md)

## What is this?

A tool that converts `.tar` files saved with `docker save` into a format for MikroTik RouterOS.

It supports OCI-compliant container images produced in a containerd-enabled environment, as well as docker-archive compatible container images.

## Usage

`oci2mtk <IN_FILE> [-d] [-f] [-p <PLATFORM>] [-t <TAG>] [-s] [-o <OUT_FILE>]`

- `<IN_FILE>` input file name — accepts `.tar.gz` in addition to `.tar`
- `-d` dry run — run the conversion without actually writing the output
- `-f` overwrite — overwrite `<OUT_FILE>` if it already exists
- `-p <PLATFORM>` platform to select (usually not needed)
- `-t <TAG>` tag to select (usually not needed)
- `-s` strip — remove the docker.io domain decoration that `docker save` adds to the tag
- `-o <OUT_FILE>` output file name — the extension can be `.tar` or `.tar.gz`

### Exit codes

- `0` success
- `1` conversion not needed
- `2` conversion failed
- `3` argument error

## Specification of container images compatible with RouterOS

The conditions below were confirmed on RouterOS 7.21. **This is not official MikroTik information.**

- `manifest.json`, `config.json`, and `layer*.tar` are top-level entries
- `oci-layout` and `index.json` are not included
- each layer image file name has a `.tar` extension

## Limitations

- Nested OCI indexes are not supported.
- zstd-compressed layers are not supported.
- More than 1,000 layers are not supported.

## License

UPL 1.0

## Author

Sakilabo Corporation Ltd.
