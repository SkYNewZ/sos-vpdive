# sos-vpdive

A small support desk for a volunteer-run diving club whose members use the
VPDive platform. Members file requests through a public form; a handful of
committee members handle them. Simplicity and robustness beat features.

Status: under development. Lot 1 (foundation and members whitelist) in progress.

## Development

Requirements: Go (version in `go.mod`), `make`, `curl`, Docker for the image.

    make test   # unit and HTTP tests
    make lint   # golangci-lint

## Dependencies

Each dependency is listed here with the reason it exists (see Task 15).

## License

MIT
