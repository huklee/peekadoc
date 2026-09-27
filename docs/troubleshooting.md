# Troubleshooting

Common problems when running peekadoc, grouped by area. If none of these match, check peekadoc's log output first: the MkDocs worker's errors are printed there too.

- [Markdown rendering (MkDocs)](#markdown-rendering-mkdocs)
- [Connecting from another device](#connecting-from-another-device)
- [Tailscale on macOS](#tailscale-on-macos)
- [Running as a service](#running-as-a-service)
- [Downloads](#downloads)

---

## Markdown rendering (MkDocs)

| Symptom | Fix |
|---|---|
| Log says `renderer did not start` | Make sure `uv` is on your `PATH`, then run the manual render command below to see the Python error. |
| Markdown pages show `Markdown render failed: …` | The worker's error is printed after the colon and in peekadoc's log. It's usually a broken `mkdocs.yml` entry or an extension whose package isn't in `mkrender.py`'s `dependencies`. |
| First start is slow | `uv` is downloading MkDocs and Material. Run `uv sync --script mkrender.py` once in advance. |
| Pages look unstyled after upgrading Material | Delete `.cache/` (the copied theme assets) and restart peekadoc. |
| Math or diagrams don't render | KaTeX and Mermaid load from a CDN, so the viewing device needs internet access. |
| Nested lists render flat, or too deeply nested | A file that mixes 2-space and 4-space list indentation can only be rendered in one style. Use one indentation style per file. |
| A "MkDocs 2.0" warning banner appears in the log | It comes from Material and is informational. `mkrender.py` pins MkDocs below 2.0. |
| An edit to `mkdocs.yml` has no effect | Restart peekadoc. Rendered pages are cached until the Markdown file itself changes. |

Render one file by hand from the repo folder to see exactly what the worker does:

```bash
printf '{"path": "%s"}\n' "$PWD/docs/prd.md" \
  | uv run --quiet --script mkrender.py --config mkdocs.yml --assets .cache/theme
```

It should print `{"ready": true}` and then `{"html": "..."}`. Anything else, such as a Python traceback on stderr or `{"error": ...}`, is the real cause.

---

## Connecting from another device

| Symptom | Fix |
|---|---|
| Page doesn't load from another device | Check that both devices are on the same tailnet (`tailscale status`) and that peekadoc was started with `-addr <tailscale-ip>:8000`, not the default `127.0.0.1`. |
| Works on the tailnet, but `http://127.0.0.1:8000` fails on the host | Expected: peekadoc listens only on the address you pass to `-addr`. Use the Tailscale IP on the host too. |
| Connection times out even with the right address | The macOS firewall may be blocking incoming connections for `peekadoc`. Allow it in System Settings → Network → Firewall. Also check your Tailscale ACLs allow the port. |
| `bind: address already in use` | Another process is using the port. Pick another port (`-addr <ip>:8001`) or find it with `lsof -i :8000`. |
| `bind: can't assign requested address` | The IP isn't on this machine, usually because Tailscale isn't connected yet. Start Tailscale first, then peekadoc. |

---

## Tailscale on macOS

The Mac App Store build of Tailscale crashes when its CLI is used from a terminal:

```
Tailscale/BundleIdentifiers.swift:47: Fatal error: The current bundleIdentifier is unknown to the registry
```

- For read-only commands, call the app's own binary: `/Applications/Tailscale.app/Contents/MacOS/Tailscale ip -4`.
- For `tailscale serve` or Tailscale SSH, switch to the standalone build: `brew install --cask tailscale`.

---

## Running as a service

| Symptom | Fix |
|---|---|
| launchd job starts, but Markdown fails | launchd starts jobs with a minimal `PATH`. Make sure the plist's `EnvironmentVariables` → `PATH` includes the folder that contains `uv` (`which uv`). |
| Job exits immediately | `WorkingDirectory` must be the repo folder, because `mkrender.py`, `mkdocs.yml` and `.cache` are resolved relative to it. Check the log file set in `StandardErrorPath`. |
| Fails at boot, works when started by hand | Tailscale may not have an IP yet. `KeepAlive` restarts peekadoc until the bind succeeds. |
| Changes to the plist aren't picked up | Unload and load it again: `launchctl bootout gui/$(id -u)/com.github.huklee.peekadoc`, then `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.github.huklee.peekadoc.plist`. |

---

## Downloads

| Symptom | Fix |
|---|---|
| No Download button or checkboxes | The server was started with `-no-download`. |
| `Selection is too large to zip` | Zips are limited to 20,000 files and 2 GiB. Select fewer or smaller folders. |
| Zip is missing some files | Hidden files and folders (names starting with `.`) and symlinks are always skipped. |
