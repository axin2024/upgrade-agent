# Pricing fixture migration

`upgrade-lab/*` are local metapackages declared in the fixture. The v2 framework
and v2 plugins all require contracts v2. Leaving either plugin on v1 retains a
conflicting requirement for contracts v1.

`Quote::estimate` floors the result at zero. Use it for sale quotes. A credit
note can go below zero and must retain its old behavior until the credit flow
has its own release. `Currency::format` preserves the representation of integer
cents and can be reviewed independently.
