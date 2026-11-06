package main

import (
	"idp-sandbox/internal/kube"
	"idp-sandbox/internal/sandbox"
)

// newKubeBackend builds the Kubernetes backend (IDP_SANDBOX_BACKEND=kubernetes). In a cluster it needs
// nothing but the service account; the variables below are for tests and special setups:
//
//	IDP_SANDBOX_NAMESPACE      namespace of workspaces and runs (default: the pod's own, else idp-sandbox)
//	IDP_SANDBOX_STORAGE_CLASS  storageClassName of the workspace PVCs (default: the cluster's)
//	IDP_SANDBOX_RUNTIME_CLASS  runtimeClassName of run pods, for example gvisor or kata
//	IDP_SANDBOX_OWNER          label value that scopes the sweep (default "default")
//	IDP_SANDBOX_KUBE_API       explicit API server URL (default: in-cluster); with IDP_SANDBOX_KUBE_TOKEN or
//	                           IDP_SANDBOX_KUBE_TOKEN_FILE and IDP_SANDBOX_KUBE_CA_FILE
func newKubeBackend(cfg sandbox.Config) (sandbox.Backend, error) {
	return kube.New(kube.Config{
		APIServer:      env("IDP_SANDBOX_KUBE_API", ""),
		Token:          env("IDP_SANDBOX_KUBE_TOKEN", ""),
		TokenFile:      env("IDP_SANDBOX_KUBE_TOKEN_FILE", ""),
		CAFile:         env("IDP_SANDBOX_KUBE_CA_FILE", ""),
		Namespace:      env("IDP_SANDBOX_NAMESPACE", ""),
		StorageClass:   env("IDP_SANDBOX_STORAGE_CLASS", ""),
		RuntimeClass:   env("IDP_SANDBOX_RUNTIME_CLASS", ""),
		Owner:          env("IDP_SANDBOX_OWNER", "default"),
		HelperImage:    cfg.Languages["shell"].Image,
		WorkspaceBytes: cfg.Limits.WorkspaceBytes,
	})
}
