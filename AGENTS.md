## XERJ code search (this repo is indexed)

This project is indexed locally as **`rosetta-gateway-docs`** and
`rosetta-gateway-web-src`. A XERJ node runs at `http://localhost:9200`.

> 索引建于 2026-10 前端 React 重写**之前**：它仍记着 `web/src/views/*.vue`、
> `web/src/router.ts`、`web/src/styles.css` 这些已删除的文件，也看不到现在的
> `web/src/**/*.tsx`。查前端代码前先重建索引（下面那条命令），否则命中的是
> 已经不存在的实现；文档侧（`DESIGN.md`、`docs/`）的命中仍然有效。

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
