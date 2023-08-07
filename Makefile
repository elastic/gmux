.PHONY: check
check: check-licenses check-fmt test

.PHONY: check-licenses
check-licenses:
	go run github.com/elastic/go-licenser -d .

.PHONY: update-licenses
update-licenses:
	go run github.com/elastic/go-licenser .

_GOIMPORTS:=$(shell go run golang.org/x/tools/cmd/goimports -l .)

.PHONY: check-fmt
check-fmt:
	@if [ -n "$(_GOIMPORTS)" ]; then printf "goimports differs: $(_GOIMPORTS) (run 'make fmt')\n" && exit 1; fi

.PHONY: fmt
fmt:
	go run golang.org/x/tools/cmd/goimports -w .

.PHONY: test
test:
	go run -modfile=tools/go.mod gotest.tools/gotestsum --format testname -- -race -v ./...
