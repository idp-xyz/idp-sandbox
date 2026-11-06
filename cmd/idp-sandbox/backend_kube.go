package main

import (
	"errors"

	"idp-sandbox/internal/sandbox"
)

// newKubeBackend builds the Kubernetes backend (IDP_SANDBOX_BACKEND=kubernetes).
func newKubeBackend(cfg sandbox.Config) (sandbox.Backend, error) {
	return nil, errors.New("the kubernetes backend is not implemented yet")
}
