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
