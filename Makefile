GOLANG_VERSION := 1.16
PWD := $(shell pwd)

fmt:
	@docker run \
		--rm \
		-v "$(PWD):/usr/src/myapp" \
		-w /usr/src/myapp \
		golang:$(GOLANG_VERSION) \
			gofmt -w plugins && go generate ./...

lint:
	@docker run \
		--rm \
		-v "$(PWD):/usr/src/myapp" \
		-w /usr/src/myapp \
		golangci/golangci-lint:v1.40.1 \
			golangci-lint run \
			-v

test:
	@docker run \
		--rm \
		-v "$(PWD):/usr/src/myapp" \
		-w /usr/src/myapp \
		golang:$(GOLANG_VERSION) \
			go test ./...
