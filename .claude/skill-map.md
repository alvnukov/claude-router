# Skill map for this repository

Invoke each skill with the Skill tool once per session, at the start of its
phase, and apply it from context afterwards. After compaction the harness
shows invoked skills again; invoke a skill anew only after /clear. Skip any
skill or tool you do not have installed.

- Planning a change: superpowers:writing-plans
- Implementing a plan: superpowers:executing-plans and
  superpowers:test-driven-development; for Go code also golang-patterns,
  golang-testing and ai-regression-testing
- A test fails or behavior surprises you: superpowers:systematic-debugging
- Before claiming done, committing or reporting:
  superpowers:verification-before-completion
- Touching credentials, API keys, user input or HTTP endpoints:
  security-review-ecc
- Before handing work off or opening a PR: superpowers:requesting-code-review
- After the review is accepted: superpowers:finishing-a-development-branch
- Go navigation: the happ MCP server, tool `code` — `op=calls` before changing
  a function, `op=diagnostics` on touched files before finishing. If happ is
  missing from your tools, it is disabled: /mcp → happ → Enable.
