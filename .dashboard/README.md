# Dashboard state files

`last-verdicts.json` — baseline snapshot updated hourly by the regression
alert task. The hourly task diffs the freshly-collected fleet verdicts
against this file, emails you if any repo flipped from CLEAN into a
regressed state, and then overwrites this file so the next hour compares
against a fresh baseline.

Do not edit by hand while the alert task is active.
