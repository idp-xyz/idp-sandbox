package kube

import (
	"fmt"
	"strings"

	"idp-sandbox/internal/sandbox"
)

const (
	sandboxUID = 65534
	stderrFile = "/tmp/idp-stderr"
	inputPath  = "/in/data"
	// MaxInputBytes is the ConfigMap limit: code and files travel in one.
	MaxInputBytes = 1 << 20
)

// jobSpec is one execution as a Job.
type jobSpec struct {
	role      string // "run" or "file"
	workspace string
	image     string
	userCmd   []string // what to run
	cmd       []string // the container command: userCmd inside the stderr wrapper
	input     []byte   // mounted read-only at /in/data (and fed to the command's stdin); nil mounts nothing
	limits    sandbox.Limits
	marker    string
	jobName   string
	deadline  int64 // activeDeadlineSeconds
}

// wrap makes the container's own stderr separable from stdout in the pod log, which merges the two:
// the command's stderr goes to a file, and after it ends the file is printed behind a per-run marker
// that the code cannot know. The exit status is the command's.
func wrap(marker string, outputBytes int, hasInput bool, errPath string, cmd []string) []string {
	redirect := ""
	if hasInput {
		redirect = " <" + inputPath
	}
	script := fmt.Sprintf(`"$@"%s 2>%s; c=$?; printf '\n--IDP-STDERR-%s--\n'; head -c %d %s; exit $c`,
		redirect, errPath, marker, outputBytes+1, errPath)
	return append([]string{"sh", "-c", script, "sh"}, cmd...)
}

func separator(marker string) string { return "\n--IDP-STDERR-" + marker + "--\n" }

func milliCPU(c float64) string {
	m := int64(c*1000 + 0.5)
	if m < 10 {
		m = 10
	}
	return fmt.Sprintf("%dm", m)
}

// jobObject builds the Job. Every field of the hardening is set explicitly rather than left to
// admission defaults: this is the security property (see TestJobIsHardened).
func (k *Kube) jobObject(js jobSpec) map[string]any {
	lim := js.limits
	labels := map[string]string{LabelKey: k.cfg.Owner, RoleLabel: js.role, WorkspaceLabel: js.workspace}
	mem := fmt.Sprint(lim.MemoryBytes)
	resources := map[string]any{
		"requests": map[string]string{"memory": mem, "cpu": milliCPU(lim.CPUs)},
		"limits":   map[string]string{"memory": mem, "cpu": milliCPU(lim.CPUs)},
	}
	mounts := []any{
		map[string]any{"name": "work", "mountPath": "/work"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
	}
	volumes := []any{
		map[string]any{"name": "work", "persistentVolumeClaim": map[string]any{"claimName": js.workspace}},
		// a memory-backed volume is a tmpfs of exactly this size (ENOSPC, not pod eviction) and counts
		// against the container's memory limit, like the Docker backend's /tmp
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": fmt.Sprint(lim.TmpBytes)}},
	}
	if js.input != nil {
		mounts = append(mounts, map[string]any{"name": "in", "mountPath": "/in", "readOnly": true})
		volumes = append(volumes, map[string]any{"name": "in", "configMap": map[string]any{"name": js.jobName, "defaultMode": 0444}})
	}
	pod := map[string]any{
		"restartPolicy":                 "Never",
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"hostNetwork":                   false,
		"hostPID":                       false,
		"hostIPC":                       false,
		"shareProcessNamespace":         false,
		"terminationGracePeriodSeconds": 1,
		"securityContext": map[string]any{
			"runAsNonRoot": true, "runAsUser": sandboxUID, "runAsGroup": sandboxUID, "fsGroup": sandboxUID,
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"containers": []any{map[string]any{
			"name": "run", "image": js.image, "imagePullPolicy": "IfNotPresent",
			"command": js.cmd, "workingDir": "/work",
			"env": []any{
				map[string]any{"name": "HOME", "value": "/tmp"},
				map[string]any{"name": "LANG", "value": "C.UTF-8"},
				map[string]any{"name": "PYTHONDONTWRITEBYTECODE", "value": "1"},
				map[string]any{"name": "PYTHONUNBUFFERED", "value": "1"},
			},
			"securityContext": map[string]any{
				"runAsNonRoot": true, "runAsUser": sandboxUID, "runAsGroup": sandboxUID,
				"privileged": false, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
				"capabilities":   map[string]any{"drop": []string{"ALL"}},
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"resources":    resources,
			"volumeMounts": mounts,
		}},
		"volumes": volumes,
	}
	if k.cfg.RuntimeClass != "" {
		pod["runtimeClassName"] = k.cfg.RuntimeClass
	}
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": js.jobName, "labels": labels},
		"spec": map[string]any{
			"backoffLimit":            0,
			"completions":             1,
			"parallelism":             1,
			"activeDeadlineSeconds":   js.deadline,
			"ttlSecondsAfterFinished": 120, // a safety net: the service deletes the Job itself
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     pod,
			},
		},
	}
}

// jobBase shortens a workspace name so that <base>-<r|f>-<8 hex> is a valid name (pod names add a suffix).
func jobBase(ws string) string {
	if len(ws) > 40 {
		ws = strings.TrimRight(ws[:40], "-")
	}
	return ws
}
