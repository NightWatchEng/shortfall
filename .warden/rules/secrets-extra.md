---
id: secrets-extra
severity: HIGH
engine: declarative
applies_to: ["**"]
excludes: [".warden/rules/**"]
checks:
  - id: dsn-embedded-password
    pattern: '(?:mysql|redis|rediss|amqp|amqps|mongodb(?:\+srv)?)://[^/\s:@"]+:[^@\s"]+@'
    message: "connection string with an embedded password — credentials come from the environment"
  - id: client-secret-assignment
    pattern: '(?i)client_secret\s*[=:]\s*["'']?[A-Za-z0-9_\-]{12,}'
    message: "hardcoded OAuth client secret — credentials come from the environment"
  - id: provider-sk-key
    pattern: 'sk-(?!ant-xxx)[A-Za-z0-9_\-]{24,}'
    message: "provider API key material in the diff — credentials come from the environment"
---
Companion to `secrets-in-diff`: the pinned mechanical checker misses these
shapes (non-Anthropic `sk-` keys, non-postgres DSNs with embedded passwords,
`client_secret` assignments), so this repo carries them as declarative
checks. Found during this repo's own enrollment review — the rule body of
`secrets-in-diff` used to promise this coverage without anything enforcing
it.

Placeholders that make a check fire in a test fixture should use clearly
fake, short values (`sk-ant-xxx`, `<YOUR_KEY>`) rather than realistic
20-plus-character strings.
