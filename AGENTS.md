## XERJ code search (this repo is indexed)

This project is indexed locally as **`rosetta-gateway-docs`** (3781 passages) and
`rosetta-gateway-web-src` (7). A XERJ node runs at `http://localhost:9200`.

Before searching or reading source, prefer the `xerj_search` MCP tool scoped to
this project's index:

```
xerj_search(index="rosetta-gateway-docs", query="<what the code does, in words>")
```

It returns the enclosing code passage with `file:line` instead of whole files.
Use `grep`/`rg` for exact literals and regexes; use XERJ for "where is the thing
that does X" questions, which is what an agent otherwise burns context on.

Re-index after significant changes (incremental, ~15s):

```powershell
%LOCALAPPDATA%\Programs\xerj\xerj-index.ps1 .
```

See `~/.codex/AGENTS.md` for the full tool list and the node's security posture.
