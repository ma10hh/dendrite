---
title: Releasing
parent: Development
permalink: /development/releasing
---

# Releasing this fork

Releases are tags of the form `vX.Y.Z-fN`: the upstream version this fork is based on, then `-f` and the number of the fork build (`v0.15.2-f2`). Publishing a GitHub release with such a tag runs `.github/workflows/fork-release.yml`, which builds the image `ghcr.io/<owner>/dendrite-monolith:<tag>` for `linux/amd64`. There is no `latest` tag, and nobody should pull one.

## CHANGES.md

Every change that a user or an administrator can notice gets a line under `## Unreleased` in `CHANGES.md`, in the same commit or pull request as the change:

- **Upgrade notes**: what has to be done by hand. A new or removed configuration key, a migration that takes long, a changed default, anything that breaks a downgrade.
- **Features**, **Fixes**, **Other**: the rest. One line each, in the language of the user ("Add ...", "Fix ..."), with the option or endpoint named.

Changes inside the code that nobody can see (refactorings, tests) do not need a line. Security fixes are named as such.

When a release is made, `## Unreleased` becomes `## Dendrite <version> (<date>)` and a new empty `## Unreleased` is put on top.

## Checklist

1. The branch to release is merged and its CI is green: unit tests on SQLite and PostgreSQL, lint, and the upgrade test (`cmd/dendrite-upgrade-tests`).
2. The lab passes: `contrib/mas-lab/run.sh` (and `SLOW=1` once in a while: it waits for a token to expire).
3. Complement: `contrib/complement-docker/run.sh` (it builds the image of the checkout and runs `./tests ./tests/csapi` of [Complement](https://github.com/matrix-org/complement) with working federation), compare the failures with the list in `docs/development/complement.md`. A test that passed before and fails now blocks the release.
4. The fuzz tests run for a few minutes each, for example `go test ./roomserver/state -run XXX -fuzz FuzzResolveStateV2 -fuzztime 5m`; the files they write in `testdata` when they find something are committed with the fix.
5. `CHANGES.md` is complete and the upgrade notes are read once more by someone who did not write them.
6. The README's *Supported features* table is still true.
7. `VersionMajor`, `VersionMinor` and `VersionPatch` in `internal/version.go` are the upstream version; do not change them for a fork build.
8. Tag, publish the release, check that the image has been built and that it starts with the sample configuration.
9. Keep the previous image: a downgrade is not supported, so the way back from a bad release is the backup taken before the upgrade, see [backup and upgrade](../administration/11_backup_and_upgrade.md).
