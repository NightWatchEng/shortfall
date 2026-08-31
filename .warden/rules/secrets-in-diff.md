---
id: secrets-in-diff
severity: HIGH
engine: python
applies_to: ["**"]
# Rule bodies quote the very patterns they hunt — never scan them.
excludes: [".warden/rules/**"]
---
Flag any ADDED line that introduces a credential or secret.

What the pinned mechanical checker (platform v1.1.0) actually fires on —
stated honestly so nobody reads protection into this file that does not
exist: Anthropic keys (`sk-ant-...`), GitHub tokens (`ghp_`,
`github_pat_`), AWS access key ids (`AKIA` + 16), JWTs, `postgres://` /
`postgresql://` connection strings with embedded passwords, and quoted
assignments to `api_key` / `secret` / `token` / `password`.

Known gaps in the mechanical net (covered by the companion declarative rule
`secrets-extra` and by pre-PR judgment review): other providers' `sk-` keys,
`mysql://` / `redis://` / `amqp://` / `mongodb://` DSNs, `client_secret`
assignments, and raw `.env` contents.

Do NOT flag: obvious placeholders (`sk-ant-xxx`, `<YOUR_KEY>`, `example`,
`redacted`), test fixtures with clearly fake values, or code that only READS
configuration from the environment.

Evidence must quote the exact offending added line.
