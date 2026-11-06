# idp-sandbox build and test entry points.
.PHONY: build vet test images test-integration e2e

build:
	go build -o bin/ ./cmd/...

vet:
	gofmt -l . | (! grep .) && go vet ./...

test: vet
	go test -race ./internal/...

# The runtime images are pulled by the operator, never by the service (a caller cannot make it fetch anything).
images:
	docker pull public.ecr.aws/docker/library/python:3.12-slim
	docker pull public.ecr.aws/docker/library/alpine:3.22

# Needs a Docker daemon and the python:3.12-slim and alpine:3.22 images (public.ecr.aws mirrors by default;
# IDP_SANDBOX_PYTHON_IMAGE / IDP_SANDBOX_SHELL_IMAGE override). Tries to escape the sandbox, for real.
test-integration:
	go test -count=1 -timeout 600s ./tests/integration/

# The sandbox behind the real idp-mcp gateway (needs ../idp-mcp and its fixture image).
e2e:
	bash deploy/e2e.sh
