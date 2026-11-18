# Security policy

Report a vulnerability privately through GitHub: **Security → Report a vulnerability** on this
repository. Do not open a public issue for it.

Include what is affected (version or commit), how to reproduce it and what an attacker gains. You
get an answer within a week. Fixes go to the latest release only.

karga-server is meant to be reached only through its local unix socket by members of the `karga`
group, who are as powerful as the Docker socket's users (see the README's security notes). Reports
about the CLI belong in github.com/hasankaradmir/karga-cli, about the package engine or the API types
in github.com/hasankaradmir/karga-sdk.
