# peekadoc

Peek at your home machine's files from anywhere on your tailnet, without being able to change anything.

peekadoc is a read-only web file browser: a folder tree on the left, and the selected file on the right. Markdown is rendered with [MkDocs Material](https://squidfunk.github.io/mkdocs-material/), HTML pages run sandboxed, and code, images and PDFs preview inline.

## Features

- Lazy folder tree and file-name search, so large folders open instantly
- On-demand Markdown rendering with MkDocs Material: math, Mermaid, tables, admonitions, code highlighting, table of contents
- Sandboxed HTML preview, syntax-highlighted code, images, PDFs
- Live reload when the file you're viewing changes on disk
- Download the current file, or multi-select files and folders and download them as a zip (disable with `-no-download`)
- Strictly read-only: `GET` only, no hidden files, nothing outside the served folder
- Deep links (`#/path/to/file.md`), dark mode, phone-friendly layout

## Requirements

- [Go](https://go.dev/) 1.25+
- [uv](https://docs.astral.sh/uv/). The MkDocs renderer's Python dependencies are installed automatically on first run.
- [Tailscale](https://tailscale.com/), to reach it from other devices

## Usage

```bash
go build -o peekadoc .
./peekadoc -root ~/Documents -addr <your-tailscale-ip>:8000
```

Then open `http://<your-tailscale-ip>:8000/` from any device on your tailnet. Run `./peekadoc -h` for all flags.

Binding to the Tailscale IP keeps it off your LAN and the internet. peekadoc has no login of its own, so anyone on your tailnet who can reach the port can read the folder; use Tailscale ACLs to narrow that down.

## How it works

A Go server handles the UI, file tree, previews and downloads. For Markdown, it drives a long-lived Python worker (`mkrender.py`) over stdin/stdout. The worker builds each page with MkDocs on demand (about 0.2 s), and the result is cached until the file changes. See [design.md](design.md) for the full design and [docs/prd.md](docs/prd.md) for the original requirements.
