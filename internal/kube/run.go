package kube

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"idp-sandbox/internal/sandbox"
)

type podInfo struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			State struct {
				Waiting    *struct{ Reason string } `json:"waiting"`
				Running    *struct{}                `json:"running"`
				Terminated *struct {
					ExitCode int    `json:"exitCode"`
					Reason   string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type jobStatus struct {
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"conditions"`
	} `json:"status"`
}

// terminal reports whether the Job finished and, if it failed, why.
func (j jobStatus) terminal() (done bool, failReason string) {
	for _, c := range j.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			return true, ""
		case "Failed":
			return true, c.Reason
		}
	}
	return false, ""
}

func (k *Kube) pods(ctx context.Context, job string) ([]podInfo, error) {
	var l struct {
		Items []podInfo `json:"items"`
	}
	q := url.Values{"labelSelector": {"job-name=" + job}}
	if err := k.c.json(ctx, http.MethodGet, k.ns("pods"), q, nil, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// pullFailures are container waiting reasons that will not clear by themselves.
var pullFailures = map[string]bool{"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "ErrImageNeverPull": true}

// cleanup removes the Job (and through it the pods) and the ConfigMap, with a context of its own so
// that a cancelled request still cleans up.
func (k *Kube) cleanup(jobName string, hasCM bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = k.delete(ctx, "jobs", jobName)
	if hasCM {
		_ = k.delete(ctx, "configmaps", jobName)
	}
}

func (k *Kube) workspaceExists(ctx context.Context, name string) error {
	if !dnsLabel.MatchString(name) || len(name) > 63 {
		return sandbox.ErrNotFound
	}
	err := k.c.json(ctx, http.MethodGet, k.ns("persistentvolumeclaims")+"/"+name, nil, nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return sandbox.ErrNotFound
	}
	return err
}

// execute creates the Job (and its ConfigMap), waits for it, and collects the result.
func (k *Kube) execute(ctx context.Context, js jobSpec, timeout time.Duration) (sandbox.RunResult, error) {
	if len(js.input) > MaxInputBytes {
		return sandbox.RunResult{}, sandbox.ErrFileTooLarge
	}
	if err := k.workspaceExists(ctx, js.workspace); err != nil {
		return sandbox.RunResult{}, err
	}
	started := time.Now()
	js.jobName = jobBase(js.workspace) + "-" + js.role[:1] + "-" + randHex(4)
	js.marker = randHex(8)
	js.cmd = wrap(js.marker, js.limits.OutputBytes, js.input != nil, stderrFile, js.userCmd)
	js.deadline = int64((timeout + k.cfg.Grace).Seconds() + 0.999)
	hasCM := js.input != nil
	defer k.cleanup(js.jobName, hasCM)

	if hasCM {
		err := k.c.json(ctx, http.MethodPost, k.ns("configmaps"), nil, map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": js.jobName, "labels": map[string]string{
				LabelKey: k.cfg.Owner, RoleLabel: js.role, WorkspaceLabel: js.workspace}},
			// binaryData: the bytes are arbitrary, `data` must be UTF-8
			"binaryData": map[string]string{"data": base64.StdEncoding.EncodeToString(js.input)},
		}, nil)
		if err != nil {
			return sandbox.RunResult{}, err
		}
	}
	if err := k.c.json(ctx, http.MethodPost, k.ns("jobs"), nil, k.jobObject(js), nil); err != nil {
		return sandbox.RunResult{}, err
	}
	timedOut, failReason, err := k.wait(ctx, js.jobName, timeout)
	if err != nil {
		return sandbox.RunResult{}, err
	}
	res := sandbox.RunResult{TimedOut: timedOut || failReason == "DeadlineExceeded"}

	pods, err := k.pods(ctx, js.jobName)
	if err != nil {
		return sandbox.RunResult{}, err
	}
	if len(pods) == 0 {
		return sandbox.RunResult{}, errors.New("the job created no pod")
	}
	pod := pods[0]
	exit, have := -1, false
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "run" && cs.State.Terminated != nil {
			exit, have = cs.State.Terminated.ExitCode, true
			res.OOMKilled = cs.State.Terminated.Reason == "OOMKilled"
		}
	}
	switch {
	case have:
		res.ExitCode = exit
	case res.TimedOut:
		res.ExitCode = 137 // killed, as the Docker backend's kill reports it
	default:
		return sandbox.RunResult{}, fmt.Errorf("the run ended without a result (job failure %q, pod phase %q)", failReason, pod.Status.Phase)
	}

	n := js.limits.OutputBytes
	limit := int64(2*(n+1) + len(separator(js.marker)))
	logText, err := k.c.text(ctx, k.ns("pods")+"/"+pod.Metadata.Name+"/log",
		url.Values{"container": {"run"}, "limitBytes": {fmt.Sprint(limit)}}, limit+1024)
	if err != nil && !res.TimedOut {
		return sandbox.RunResult{}, err
	}
	res.Stdout, res.Stderr, res.Truncated = splitLog(logText, js.marker, n)
	res.Duration = time.Since(started)
	return res, nil
}

// wait polls the Job until it finishes. The run's own clock starts when the container is first seen
// running (scheduling and image pull do not count; activeDeadlineSeconds is the backstop for those): past
// the timeout it returns timedOut and the caller's cleanup kills the pod.
func (k *Kube) wait(ctx context.Context, job string, timeout time.Duration) (timedOut bool, failReason string, err error) {
	var runningSince time.Time
	hard := time.NewTimer(timeout + k.cfg.Grace + 15*time.Second)
	defer hard.Stop()
	tick := time.NewTicker(k.cfg.Poll)
	defer tick.Stop()
	for {
		var j jobStatus
		if err := k.c.json(ctx, http.MethodGet, k.ns("jobs")+"/"+job, nil, nil, &j); err != nil {
			return false, "", err
		}
		if done, reason := j.terminal(); done {
			return false, reason, nil
		}
		pods, err := k.pods(ctx, job)
		if err != nil {
			return false, "", err
		}
		for _, p := range pods {
			for _, cs := range p.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && pullFailures[w.Reason] {
					return false, "", fmt.Errorf("the pod cannot start its container (%s)", w.Reason)
				}
				if cs.State.Running != nil && runningSince.IsZero() {
					runningSince = time.Now()
				}
			}
		}
		if !runningSince.IsZero() && time.Since(runningSince) > timeout {
			return true, "", nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-hard.C:
			return false, "", errors.New("the job did not finish in time")
		case <-tick.C:
		}
	}
}

// splitLog separates the merged pod log at the marker into stdout and stderr, each at most n bytes.
func splitLog(raw, marker string, n int) (stdout, stderr string, truncated bool) {
	sep := separator(marker)
	if i := strings.Index(raw, sep); i >= 0 {
		stdout, stderr = raw[:i], raw[i+len(sep):]
	} else {
		stdout = raw // the command was killed before the marker, or its output overflowed the read limit
	}
	if len(stdout) > n {
		stdout, truncated = stdout[:n], true
	}
	if len(stderr) > n {
		stderr, truncated = stderr[:n], true
	}
	return
}

// Run executes spec.Cmd in spec.Image with the workspace at /work.
func (k *Kube) Run(ctx context.Context, spec sandbox.RunSpec) (sandbox.RunResult, error) {
	if len(spec.Cmd) == 0 {
		return sandbox.RunResult{}, fmt.Errorf("%w: no command", sandbox.ErrInvalid)
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = spec.Limits.Timeout
	}
	return k.execute(ctx, jobSpec{role: "run", workspace: spec.Workspace, image: spec.Image, userCmd: spec.Cmd,
		input: spec.Stdin, limits: spec.Limits}, timeout)
}
