set dotenv-load

default:
    @just --list

[group('quality')]
fmt:
    golangci-lint fmt

[group('quality')]
tidy:
    go mod tidy

[group('quality')]
generate:
    go generate ./...

[group('quality')]
lint:
    golangci-lint run --timeout 5m

[group('quality')]
audit:
    govulncheck ./...

[group('quality')]
lint-ci:
    actionlint

[group('test')]
tests:
    go test -race -shuffle=on -covermode=atomic -coverprofile=coverage.out ./...

[group('test')]
test: tests

[group('build')]
build:
    go build -trimpath -o skillloop .

[group('release')]
release-check:
    goreleaser check

[group('release')]
release-local:
    goreleaser release --snapshot --clean

[group('release')]
release:
    goreleaser release --clean

pre-commit: fmt tidy generate lint tests build release-check
pc: pre-commit

check: pre-commit audit lint-ci
ci: check
