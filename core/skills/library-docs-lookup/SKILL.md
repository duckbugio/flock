---
name: library-docs-lookup
description: When writing code against an external library, framework, SDK, or API, look up its CURRENT documentation with the context7 MCP tools instead of relying on memory. Use whenever you import/call a third-party package or hit an unfamiliar or fast-moving API.
---

# Look up real library docs (don't guess the API)

Training data goes stale: library APIs, method signatures, config keys, and best
practices change between versions. Guessing them is a top source of subtly-wrong
code. When you touch an external library/framework/SDK/API, get the CURRENT docs.

## How

The `context7` MCP server is available. Use its tools:

1. **`resolve-library-id`** — resolve the library name (e.g. "tauri",
   "centrifuge-go", "react-query") to its context7 id.
2. **`get-library-docs`** — fetch the up-to-date docs for that id, scoped to the
   topic/symbol you need (the specific hook, method, or config option).

Pull the version the project actually depends on — check `go.mod` /
`package.json` / `Cargo.toml` / `requirements.txt` — not "latest", when they
differ.

## When to use it

- Calling a method/option you're not 100% sure exists in the version in use.
- Wiring up a new dependency, an SDK client, or a framework feature.
- An unfamiliar or fast-moving library (cloud SDKs, web frameworks, build tools).
- A compile/runtime error that looks like an API mismatch.

## When NOT to bother

- The standard library or a tiny, stable utility you know cold.
- Pure project-internal code — read the repo, not external docs.

Prefer one focused docs lookup over a wrong guess you then have to debug.
