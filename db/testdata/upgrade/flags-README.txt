What the flags-v0.5.*.txt files are
-----------------------------------
flags-v0.5.9m.txt, flags-v0.5.10m.txt, flags-v0.5.11m.txt and flags-v0.5.12m.txt were produced
by a grep for `const flag = "..."` in server/ at each tag. They hold the 19 settings keys that the
one-time seeders of server/ write through a constant named flag.

They are NOT the complete list of the settings keys a startup writes, and they must not be edited
into one: they stay the output of that grep.

The six other startup keys (the 19 above plus these six are the 25 startup keys that the settings
rows of the data fixtures and server/upgrade_seed_test.go start from). One key per line, then
file:line at this commit; TestUpgradeFixtureSettingsAreTheStartupKeys checks that each line holds
the key:

finding_workflow_tools_v3_host_search_description server/finding_workflow.go:18
interactive_shell_default_v1 db/db.go:286
asset_intercept_default_rules_v1 db/db.go:331
intercept_default_rules_v1 db/db.go:387
intercept_default_rules_v2 db/db.go:582
intercept_default_rules_v3 db/db.go:640
