# Contributing

Bug reports, documentation improvements, and focused pull requests are welcome.
Discuss large changes in an issue before implementing them.

## Development

Use Go 1.22 or newer. There are no third-party dependencies.

```sh
gofmt -w cmd compressor
go vet ./...
go test -race ./...
go build -o bin/imgcompress ./cmd/imgcompress
```

Add regression tests for behavior changes. Generate small image fixtures in tests
where possible. Check decoded pixels for lossless changes and file sizes and PSNR
for compression changes. Keep output deterministic and preserve GIF bytes.

## Issues and pull requests

Include your Go version, operating system, exact command, expected behavior, and
actual result. Only attach images you have permission to share; remove sensitive
metadata first. Explain the problem, the change, and how you tested it in your PR.
Keep discussion respectful and constructive. Contributions are under the project's
[MIT license](LICENSE).

## Publishing the repository

The current module name is `imagecompressor`, so the documented build and package
examples work from a local checkout. Once the GitHub owner and repository name are
chosen, change the module in `go.mod` to `github.com/OWNER/REPOSITORY`, update the
import in `cmd/imgcompress/main.go` and both README examples, and rerun the checks.
Then remote `go install github.com/OWNER/REPOSITORY/cmd/imgcompress@latest` can be
documented. Do not commit generated binaries; `.gitignore` excludes them.
