.PHONY: build run test clean

build:
	docker build -t quirkproxy:v1 .

run:
	docker compose up -d

test:
	curl -s http://127.0.0.1:18080/healthz | jq .

clean:
	docker rm -f quirkproxy quirkproxy-test 2>/dev/null || true
