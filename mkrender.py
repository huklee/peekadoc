#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "mkdocs>=1.6,<2",
#     "mkdocs-material>=9.5",
#     "mdx-truly-sane-lists>=1.3",
# ]
# ///
"""MkDocs Material renderer worker for peekadoc.

Started and owned by the Go server. It has no network listener: it reads one JSON
request per line on stdin and writes one JSON response per line on stdout.

    startup:  {"ready": true}
    request:  {"path": "/absolute/path/to/file.md"}
    response: {"html": "<!doctype html>..."}  or  {"error": "..."}

Each request builds a one-page MkDocs site in a temp dir using --config, and returns
its HTML with theme asset URLs pointed at /mk/_theme/ (copied once into --assets).
"""
from __future__ import annotations

import argparse
import json
import logging
import re
import shutil
import sys
import tempfile
from pathlib import Path

KATEX = "https://cdn.jsdelivr.net/npm/katex@0.16/dist"

# peekadoc shows its own file tree and breadcrumbs, so hide Material's header/nav.
INJECT = f"""
<style>
  .md-header, .md-tabs, .md-sidebar--primary, .md-footer {{ display: none !important; }}
  .md-main__inner {{ margin-top: 0; }}
  .md-sidebar--secondary {{ top: 0; }}
</style>
<link rel="stylesheet" href="{KATEX}/katex.min.css">
<script defer src="{KATEX}/katex.min.js"></script>
<script defer src="{KATEX}/contrib/auto-render.min.js"
  onload="document.querySelectorAll('.arithmatex').forEach(el => renderMathInElement(el, {{
    delimiters: [{{left: '\\\\(', right: '\\\\)', display: false}}, {{left: '\\\\[', right: '\\\\]', display: true}}],
    throwOnError: false }}))"></script>
"""

ASSET_URL = re.compile(r'(\b(?:href|src)=")assets/')
# A list item indented by exactly two spaces means GitHub-style nesting. Python-Markdown
# needs 4-space nesting by default and mdx_truly_sane_lists needs 2, and neither handles
# both, so the list extension is chosen per file.
TWO_SPACE_ITEM = re.compile(r"^ {2}(?:[-*+]|\d+[.)]) ", re.MULTILINE)


def render(src: Path, config: Path, assets: Path, copy_assets: bool) -> str:
    from mkdocs.commands.build import build
    from mkdocs.config import load_config

    with tempfile.TemporaryDirectory(prefix="peekadoc-") as tmp:
        docs = Path(tmp, "docs")
        docs.mkdir()
        text = src.read_text(encoding="utf-8", errors="replace")
        (docs / "index.md").write_text(text, encoding="utf-8")
        site = Path(tmp, "site")
        cfg = load_config(config_file=str(config), docs_dir=str(docs), site_dir=str(site))
        if TWO_SPACE_ITEM.search(text):
            cfg["markdown_extensions"].append("mdx_truly_sane_lists")
        build(cfg)
        if copy_assets:
            shutil.copytree(site / "assets", assets / "assets", dirs_exist_ok=True)
        html = (site / "index.html").read_text(encoding="utf-8")
    html = ASSET_URL.sub(r"\1/mk/_theme/assets/", html)
    return html.replace("</head>", INJECT + "</head>", 1)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--config", type=Path, required=True, help="MkDocs config (theme + markdown_extensions)")
    ap.add_argument("--assets", type=Path, required=True, help="folder to copy Material theme assets into")
    args = ap.parse_args()

    # stdout is the protocol channel; send anything else (MkDocs/Material banners) to stderr.
    proto, sys.stdout = sys.stdout, sys.stderr
    logging.getLogger("mkdocs").setLevel(logging.ERROR)

    def reply(obj: dict):
        proto.write(json.dumps(obj, ensure_ascii=False) + "\n")
        proto.flush()

    import mkdocs.commands.build  # noqa: F401  (import before signalling ready)

    reply({"ready": True})
    assets_copied = False
    for line in sys.stdin:
        try:
            req = json.loads(line)
            html = render(Path(req["path"]), args.config, args.assets, copy_assets=not assets_copied)
            assets_copied = True
            reply({"html": html})
        except Exception as e:  # report and keep serving
            reply({"error": f"{type(e).__name__}: {e}"})


if __name__ == "__main__":
    main()
