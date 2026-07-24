# Contributing

Use [Conventional Commits](https://www.conventionalcommits.org/) for every
commit. Keep each commit focused and use an appropriate type such as `feat`,
`fix`, `docs`, `test`, `refactor`, or `chore`.

Run the project checks before pushing:

```console
go test ./...
go vet ./...
nix build --no-link
```

Run the parser benchmarks with:

```console
go test -run '^$' -bench . -benchmem
```

The fuzz targets run their seed corpus as part of the normal test suite. For a
longer fuzzing session, select a target explicitly, for example:

```console
go test -run '^$' -fuzz FuzzReadField -fuzztime 30s
```

Releases are intentionally manual. Build the release artifact locally with
Nix, test that exact binary, generate its checksum, and attach both files
directly to the corresponding GitHub release. The repository does not use an
automated Nix installation or release workflow.
