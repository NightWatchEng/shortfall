---
id: os-command-injection
severity: HIGH
engine: declarative
implements: [os-command-injection]
applies_to: ["**/*.go"]
checks:
  - id: shell-dash-c
    pattern: 'exec\.Command\(\s*"(sh|bash|/bin/sh|/bin/bash)"\s*,\s*"-c"'
    message: "command string runs via a shell — pass an argv list to exec.Command instead (CWE-78)"
---
Catalog entry CWE-78, adopted 2026-08-29 (workspace-a74) with the starter
adapted from its Python idioms (shell=True, os.system) to the Go analog:
a shell invoked with -c turns any interpolated byte into a second
command. The tree shells out only in test harnesses today and every call
passes argv lists; measured matches at adoption: 0. Constant strings
through a shell still flag — switch to argv or dismiss in one line, per
the catalog's own cost note.
