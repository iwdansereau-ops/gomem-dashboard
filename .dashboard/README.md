# Dashboard state files

`last-verdicts.json` — baseline snapshot updated hourly by the regression
alert task. The hourly task diffs the freshly-collected fleet verdicts
against this file, emails you if any repo flipped from CLEAN into a
regressed state, and then overwrites this file so the next hour compares
against a fresh baseline.

Do not edit by hand while the alert task is active.

**Privacy invariant:** this file is committed to a public repository, so it must
contain **public repositories only**. Every entry carries `"private": false`.
`collect-verdicts.sh` excludes private repos by default, and
`scripts/ci/assert-public-only.py last-verdicts.json --require-flag` gates
against any private repo (or any entry missing the `private` flag) slipping in.
Run that check before committing a regenerated baseline.
