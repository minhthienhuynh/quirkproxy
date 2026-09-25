BINARY := quirkproxy
PKG := ./...
PORT := 8080

.PHONY: build test race vet fmt run smoke clean

build:
	go build -o $(BINARY) .

test:
	go test $(PKG)

race:
	go test -race -count=1 $(PKG)

vet:
	go vet $(PKG)

fmt:
	gofmt -l .
	@test -z "$$(gofmt -l .)" || (echo "gofmt dirty"; exit 1)

run: build
	./$(BINARY) -config config.sample.json

smoke: build
	@./$(BINARY) -config config.sample.json > /tmp/qp-smoke.log 2>&1 & echo $$! > /tmp/qp.pid; \
	sleep 6; \
	if curl -sf http://127.0.0.1:$(PORT)/healthz > /dev/null && \
	   curl -sf http://127.0.0.1:$(PORT)/v1/models > /dev/null; then \
		echo "smoke OK"; code=0; \
	else \
		echo "smoke FAIL (see /tmp/qp-smoke.log)"; code=1; \
	fi; \
	kill $$(cat /tmp/qp.pid) 2>/dev/null; exit $$code

clean:
	rm -f $(BINARY)
	go clean -testcache
