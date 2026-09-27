# peekadoc — System Design

Read-only web file browser that lets a remote client (over Tailscale) browse a folder on a home machine and view files rendered on demand. Markdown is rendered with MkDocs Material.

---

## 1. Goals and constraints

Derived from [`prd.md`](prd.md):

| Requirement | How peekadoc meets it |
|---|---|
| View results on the host from a remote client | HTTP server bound to the host's Tailscale IP |
| Nothing flows back from the client to the host | Only `GET`/`HEAD` routes exist; no upload, edit or delete |
| No exposure to LAN / internet | Binds to the Tailscale interface address only |
| MkDocs-quality Markdown (math, tables, Mermaid, highlighting) | Each `.md` file is rendered by MkDocs Material |
| Browse large folders without pre-building a site | Folder tree loads lazily; each file is rendered only when opened |
| Minimal traces on the client (MDM) | `-no-download` turns off download buttons and zip downloads |

### Non-goals
* Editing, uploading or syncing files.
* Full-text search of file contents (only names are searched).
* In-app authentication (access control is delegated to Tailscale, see §7).

### History
1. **MkDocs over the whole folder**: pre-rendering `~/Work` (7 GB, ~26k files, ~600 `.md`) copied 4.6 GB of assets per build and pulled in `node_modules` READMEs.
2. **Python `http.server` + markdown-it**: on-demand, but not MkDocs output, and `http.server` is not meant for long-running use.
3. **Current**: Go server for everything except Markdown; MkDocs runs as a Python worker process with no network listener.

---

## 2. Architecture

```
 Client (browser on the tailnet)                 Host
┌──────────────────────────────────┐          ┌──────────────────────────────────────────┐
│ app.html (vanilla JS)            │          │ peekadoc  (Go, net/http)                 │
│ ┌────────────┬─────────────────┐ │   HTTP   │                                          │
│ │ Tree panel │ Viewer panel    │ │  over    │  /api/list /api/view /api/stat /api/find │
│ │ (lazy)     │ md → MkDocs     │◄├──────────┤► /raw/<path>   files (Range, download)   │
│ │ + search   │ html → sandbox  │ │ Tailscale│  /zip  /api/zipinfo   multi-file zip     │
│ │ + select   │ code/img/pdf    │ │          │  /mk/<path>.md ──┐                       │
│ └────────────┴─────────────────┘ │          │  /mk/_theme/*    │ (shared theme assets) │
└──────────────────────────────────┘          │                  │ JSON lines            │
                                              │                  ▼ stdin/stdout          │
                                              │   mkrender.py (Python via uv)            │
                                              │   MkDocs Material, one page per request  │
                                              │                  │                       │
                                              │                  ▼ read-only             │
                                              │           --root folder                  │
                                              └──────────────────────────────────────────┘
```

| File | Role |
|---|---|
| `main.go` | Flags, routes, HTTP server, graceful shutdown |
| `browse.go` | Path sandboxing, listing, view kinds, name search, raw files, zip downloads, code highlighting (chroma) |
| `render.go` | Starts and supervises the MkDocs worker, request/response over pipes, page cache |
| `app.html` | Single-page UI, embedded into the binary with `go:embed` |
| `mkrender.py` | MkDocs Material worker (PEP 723 inline dependencies, run by `uv`) |
| `mkdocs.yml` | Theme and Markdown extensions used for every page |

---

## 3. Go server

### 3.1 Routes

All routes are `GET` (Go's mux also answers `HEAD`); anything else gets `405`.

| Route | Params | Response |
|---|---|---|
| `/` | — | `app.html` |
| `/chroma.css` | — | Code-highlight CSS (`github` light, `github-dark` under `prefers-color-scheme`) |
| `/api/list` | `p` folder | `{root, path, download, entries: [{name, dir}]}`, folders first, dotfiles hidden |
| `/api/view` | `p` path | View kind, see §3.2 |
| `/api/stat` | `p` path | `{mtime, size}`, polled by the client for live reload |
| `/api/find` | `q` (≥ 2 chars) | `{hits: [{path, dir}], truncated}` |
| `/raw/<path>` | `download=1` optional | File bytes via `http.ServeContent` (Range, conditional requests); attachment when `download` is set |
| `/api/zipinfo` | `p` repeated | `{name, files, bytes}` or an error, checked before a zip download starts |
| `/zip` | `p` repeated | Streamed zip of the selected files and folders |
| `/mk/<path>.md` | — | MkDocs-rendered page (from the worker, cached) |
| `/mk/<folder>/` | — | Redirect to its `README.md` / `readme.md` / `index.md` |
| `/mk/<other path>` | — | Raw file, so relative images and links inside a rendered page resolve |
| `/mk/_theme/…` | — | Shared Material assets copied once by the worker |

### 3.2 View kinds (`/api/view`)

| Condition | Kind | Client shows |
|---|---|---|
| Folder with a README | `md` (`dir: true`) | MkDocs frame of the README |
| Folder without one | `dir` | Message |
| `.md`, `.markdown` | `md` | `<iframe src="/mk/…">` |
| `.html`, `.htm` | `html` | Sandboxed `<iframe src="/raw/…">` |
| Image extensions | `image` | `<img src="/raw/…">` |
| `.pdf` | `pdf` | `<iframe src="/raw/…">` (browser PDF viewer) |
| > 1 MiB | `binary` | "No preview" with size |
| Valid UTF-8, no NUL in first 8 KiB | `text` | chroma-highlighted HTML |
| Otherwise | `binary` | "No preview" with size |

### 3.3 Name search
`filepath.WalkDir` from the root (does not follow symlinks), pruning dot-folders and `node_modules`, `__pycache__`, `site-packages`, `venv`. Case-insensitive substring match on names; stops at 200 hits or 3 seconds.

### 3.4 Zip downloads
* Selected files and folders are expanded (folders recursively, skipping dotfiles and symlinks) and de-duplicated.
* Entry names are relative to the deepest common parent of the selection, so `a/b/` + `a/c.md` becomes `b/…` and `c.md`.
* Limits: 20,000 files and 2 GiB; `/api/zipinfo` reports the error before the download starts.
* Already-compressed formats (images, PDF, archives, media, Office) are stored; everything else is deflated.

---

## 4. MkDocs worker (`mkrender.py`)

### 4.1 Process model
* Started by the Go server at startup (`uv run --script mkrender.py --config mkdocs.yml --assets .cache/theme`) in its own process group.
* **No network listener.** Protocol is JSON lines over stdin/stdout:
  * startup: `{"ready": true}`
  * request: `{"path": "/absolute/file.md"}`
  * response: `{"html": "…"}` or `{"error": "…"}`
* The Python side moves `sys.stdout` to stderr so MkDocs/Material messages can't corrupt the protocol.
* Go serializes requests (one worker), applies a 60 s timeout, restarts the worker once if it died, and on shutdown closes stdin (the worker exits on EOF), then kills the process group after 2 s.

### 4.2 Rendering a page
1. Copy the file to a temp `docs/index.md`.
2. Load `mkdocs.yml` with `docs_dir` / `site_dir` pointed at the temp dir (`plugins: []`, `use_directory_urls: false`).
3. If the file has a list item indented by exactly two spaces, add `mdx_truly_sane_lists` (GitHub-style nesting). Python-Markdown's default handles 4-space nesting, the extension handles 2-space; neither handles both, so this is decided per file.
4. `mkdocs build`, read `index.html`, rewrite `assets/…` URLs to `/mk/_theme/assets/…`; theme assets are copied to `.cache/theme` on the first build only.
5. Inject CSS that hides Material's header, tabs, left nav and footer (peekadoc has its own tree and breadcrumbs; the right-hand table of contents stays), plus KaTeX auto-render for `pymdownx.arithmatex`.

Measured: ~0.2–0.4 s per uncached page; cached pages are served from Go memory (keyed by path, mtime and size; up to 256 entries).

### 4.3 Markdown features (`mkdocs.yml`)
Tables, admonitions, footnotes, definition lists, abbreviations, attribute lists, `md_in_html`, TOC permalinks, `pymdownx` highlight / inlinehilite / superfences (Mermaid) / tasklist / details / tilde / magiclink / arithmatex. Light/dark palette follows the OS.

---

## 5. Client (`app.html`)

* **Layout**: resizable sidebar (tree, search, selection bar) and viewer with a sticky breadcrumb bar and Download button. On phones the sidebar becomes a drawer.
* **Routing**: the current path is in the URL hash (`#/a/b/c.md`), so back/forward, reload and bookmarks work; the tree auto-expands to it.
* **Markdown frame**: same-origin iframe. After load, clicks on internal links are intercepted and routed through the app (tree and address bar stay in sync); external links open in a new tab; `#anchor` links scroll inside the frame. `/` in the frame focuses search.
* **HTML frame**: `sandbox="allow-scripts allow-forms allow-modals allow-popups allow-popups-to-escape-sandbox"`, with no `allow-same-origin`. Page scripts run, but in an opaque origin.
* **Live reload**: every 2 s (only while the tab is visible) the client polls `/api/stat` for the shown file and reloads the view when `mtime` changes, keeping scroll position.
* **Selection**: checkbox on each row (visible on hover or once anything is selected) or Cmd/Ctrl-click. The selection bar shows the count and downloads one file directly or several as a zip. `Esc` clears.
* **Search**: 200 ms debounce; results temporarily replace the tree. `/` focuses, `Enter` opens the first hit, `Esc` clears.

---

## 6. Dependencies

| Side | Dependency |
|---|---|
| Go server | Go ≥ 1.25, `github.com/alecthomas/chroma/v2` |
| Worker | `uv`; Python ≥ 3.11; `mkdocs` 1.6.x (< 2), `mkdocs-material`, `mdx-truly-sane-lists` (installed by `uv` from the script header) |
| Client | `github-markdown-css@5` (jsdelivr) for code/text previews; KaTeX (jsdelivr) and Mermaid (loaded by Material) inside Markdown pages. The client needs internet access. |

MkDocs is pinned below 2.0: the Material team has said MkDocs 2.0 drops the plugin and theming systems Material depends on.

---

## 7. Security model

| Concern | Control |
|---|---|
| Network exposure | Bind to the Tailscale IP only; reachable by tailnet devices subject to Tailscale ACLs. The worker has no socket at all. |
| Writes | No write routes exist. |
| Path traversal | Paths are split on `/`; any segment starting with `.` (covers `..` and dotfiles) → 404. The joined path is resolved with `filepath.EvalSymlinks` and must be the root or inside it → otherwise 403, which also blocks symlinks pointing outside. Go's mux additionally cleans `..` in URLs. |
| Hidden files | Dot-prefixed entries are never listed, searched, served or zipped. |
| Active content | `/raw/` sends `X-Content-Type-Options: nosniff`. HTML is sent with `Content-Security-Policy: sandbox allow-scripts …` (opaque origin, can't read the app's API or storage); SVG/XML get `sandbox` with no scripting. |
| Markdown frame | Same-origin by design (needed for link interception). It only contains MkDocs output of the owner's own files; raw HTML in Markdown is passed through by Python-Markdown. |
| Downloads | On by default; `-no-download` hides the UI and rejects `?download=1`, `/zip` and `/api/zipinfo`. `/raw/` stays available because previews need it, so a determined user can still save a file. |
| Authentication | None in-app; anyone on the tailnet who can reach the port can read the root folder. |

---

## 8. Operations

### Build and run
```bash
go build -o peekadoc .
./peekadoc -root ~/Work -addr <tailscale-ip>:8000
```

| Flag | Default | Meaning |
|---|---|---|
| `-root` | `.` | Folder to serve |
| `-addr` | `127.0.0.1:8000` | Listen address; use the host's Tailscale IP to expose it to the tailnet only |
| `-renderer` | `mkrender.py` | Worker script |
| `-mkdocs-config` | `mkdocs.yml` | MkDocs config |
| `-cache` | `.cache` | Theme asset folder |
| `-no-download` | `false` | View-only mode |

Relative defaults are resolved against the working directory, so run it from the repo folder. The first start may take a while as `uv` installs MkDocs. Run it in `tmux` or as a `launchd` agent so it survives SSH disconnects.

### Access
* `http://<tailscale-ip>:8000/` from any tailnet device, or `http://<machine-name>:8000/` with MagicDNS.
* Binding to the Tailscale IP means `127.0.0.1:8000` does not work on the host itself.

### Known environment issue
The Mac App Store build of Tailscale crashes when its CLI is used from a terminal (`BundleIdentifiers.swift: Fatal error`). Calling `/Applications/Tailscale.app/Contents/MacOS/Tailscale` works for read-only commands; switch to `brew install --cask tailscale` for `tailscale serve` or Tailscale SSH.

---

## 9. Known limitations and next steps

| Limitation | Possible next step |
|---|---|
| Not a persistent service | `launchd` agent in `~/Library/LaunchAgents` |
| Plain HTTP (Tailscale's WireGuard still encrypts the traffic) | `tailscale serve` for HTTPS, then bind to `127.0.0.1` |
| Client needs internet for CDN assets | Vendor github-markdown-css / KaTeX / Mermaid into the binary |
| One MkDocs worker, requests serialized | A small pool of workers if several people browse at once |
| Mixed 2- and 4-space list indentation in one file renders one style wrong | Normalize list indentation before rendering |
| Name search only | Content search via `ripgrep` with the same path rules |
| No `.ipynb` / Office previews | Render notebooks with `nbconvert` |
| No in-app auth | Rely on Tailscale ACLs; optionally restrict the port to specific devices |
