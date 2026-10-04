# Build and test person-album. It runs in docker compose; this is for
# working on it locally.
#
#   make check   # fmt + vet + test

PKG := ./src
GO  ?= go

.PHONY: check fmt vet test

check: fmt vet test

fmt:
	$(GO) fmt $(PKG)

vet:
	$(GO) vet $(PKG)

test:
	$(GO) test $(PKG)
