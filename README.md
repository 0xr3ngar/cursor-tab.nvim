# cursor-tab.nvim

Brings Cursor's AI-powered tab completion (NES / Next Edit Suggestion) to Neovim. Get intelligent multi-line code suggestions as you type and accept them with Tab — including chained edits that jump across your file, just like Cursor IDE.

> **Experimental** — Under active development. Contributions welcome.

## Features

- **Inline tab completion** — Ghost text suggestions as you type, accept with Tab
- **NES (Next Edit Suggestion)** — Multi-location chained edits with diff view: red strikethrough for deletions, green for insertions
- **Smart intent detection** — Adapts behavior based on context: typing, line changes (dd/dw/x/p), or cursor predictions
- **Cross-file context** — Sends open buffers, visible ranges, and recently viewed files to improve suggestion quality
- **LSP diagnostic awareness** — Forwards Neovim LSP diagnostics (errors, warnings, hints) to the API for context-aware fixes
- **Diff history tracking** — Records accepted edits (sliding window of 3 per file) so the API understands your editing patterns
- **Cancel-and-replace** — New keystrokes instantly cancel in-flight requests and start fresh
- **Normal mode support** — Get suggestions after normal mode edits (dd, dw, x, p) and accept with Tab

## Requirements

**System:**
- **macOS only** (reads Cursor auth from macOS-specific paths)
- curl (for HTTP requests and binary download)
- sqlite3 (to read Cursor credentials)

**Critical:**
- **Cursor IDE must be installed** at `/Applications/Cursor.app`
- **You must be signed into Cursor** (the plugin reads your auth token automatically)

Without Cursor installed and authenticated, the plugin cannot function.

## Installation

### lazy.nvim
```lua
{
  "bengu3/cursor-tab.nvim",
  config = function()
    require("cursor-tab").setup()
  end,
}
```

The plugin automatically downloads the server binary for your platform on first run.

### packer.nvim
```lua
use {
  "bengu3/cursor-tab.nvim",
  config = function()
    require("cursor-tab").setup()
  end
}
```

### vim-plug
```vim
Plug 'bengu3/cursor-tab.nvim'
```

Add to your `init.lua`:
```lua
require("cursor-tab").setup()
```

### Custom Server Path (Optional)
```lua
require("cursor-tab").setup({
  server_path = "/custom/path/to/cursor-tab-server"
})
```

### Building from Source

```bash
# Requirements: Go 1.21+, buf CLI, make
git clone https://github.com/bengu3/cursor-tab.nvim
cd cursor-tab.nvim
make build
```

Or run `:CursorTabInstall` in Neovim to retry auto-installation.

## Keymaps

| Key | Mode | Action |
|-----|------|--------|
| `Tab` | Insert | Accept suggestion and jump to next edit location |
| `Tab` | Normal | Accept NES suggestion |
| `Escape` | Normal | Dismiss active suggestion |

## Architecture

```
┌──────────────┐     HTTP/JSON      ┌──────────────┐   gRPC/connectrpc   ┌──────────────────┐
│   Neovim     │ ◄───────────────► │  Go Server   │ ◄─────────────────► │  api4.cursor.sh  │
│   (Lua)      │                    │  (local)     │                     │  (Cursor API)    │
└──────────────┘                    └──────────────┘                     └──────────────────┘
```

**Lua plugin** — Collects buffer contents, cursor position, LSP diagnostics, open files, and file metadata. Renders ghost text and NES diff views. Handles Tab/Escape keymaps and suggestion lifecycle.

**Go server** — Local HTTP server that authenticates with Cursor, builds protobuf requests with full context (diagnostics, diff history, additional files), streams responses via connectrpc, and manages suggestion chaining with a background goroutine.

### What gets sent to the API

Each completion request includes:

| Data | Purpose |
|------|---------|
| File contents + cursor position | Primary context |
| Language ID, line count, workspace path | File metadata |
| LSP diagnostics (errors, warnings, hints with ranges) | Error-aware suggestions |
| Open buffer paths + visible ranges | Cross-file context |
| Diff history (last 3 accepted edits per file) | Edit pattern understanding |
| Intent source (typing / line_changed / cursor_prediction) | Behavior adaptation |
| File version (monotonic counter) | Staleness detection |
| Line ending format | Platform-correct output |
| x-cursor-checksum header | Authentication |

## Config

Toggle on/off at runtime:

```
:CursorTab toggle
```

## Debugging

Debug logs are written to `/tmp/cursor-tab-debug.log` (Lua) and `/tmp/cursor-tab.log` (Go server). Tail them while editing:

```bash
tail -f /tmp/cursor-tab-debug.log /tmp/cursor-tab.log
```

## License

MIT
