package agentworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type commandRunner func(context.Context, []byte, int64, ...string) ([]byte, error)

type engineError struct{ code int }

func (e *engineError) Error() string {
	return fmt.Sprintf("OCI engine command failed (exit %d; output suppressed)", e.code)
}

type limitedOutput struct {
	bytes.Buffer
	limit  int64
	cancel context.CancelFunc
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.limit {
		b.cancel()
		return 0, fmt.Errorf("OCI output exceeds limit")
	}
	return b.Buffer.Write(p)
}

// OCIBackend uses an already-installed engine and never pulls or builds images.
// Both command duration and captured output are bounded; stderr is not returned
// because engines can echo stdin, environment or connection credentials.
type OCIBackend struct{ Engine string }

func (b OCIBackend) command(ctx context.Context, input []byte, limit int64, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Engine, args...)
	for _, key := range []string{"PATH", "HOME", "XDG_RUNTIME_DIR", "CONTAINER_HOST", "CONTAINER_CONNECTION", "SSH_AUTH_SOCK"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	cmd.Stdin = bytes.NewReader(input)
	out := &limitedOutput{limit: limit, cancel: cancel}
	errout := &limitedOutput{limit: 64 * 1024, cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, errout
	if err := cmd.Run(); err != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		return nil, &engineError{code: code}
	}
	return out.Bytes(), nil
}

type containerState struct {
	ID     string `json:"Id"`
	Image  string
	Config struct{ Labels map[string]string }
	State  struct {
		Status    string
		Running   bool
		ExitCode  int
		StartedAt time.Time
	}
}

func imageID(value string) string {
	value = "sha256:" + strings.TrimPrefix(value, "sha256:")
	if !digestPattern.MatchString(value) {
		return ""
	}
	return value
}

func (w *Worker) inspect(ctx context.Context, name string) (containerState, bool, error) {
	_, err := w.exec(ctx, nil, 1024, "container", "exists", name)
	if err != nil {
		var exit *engineError
		if errors.As(err, &exit) && exit.code == 1 {
			return containerState{}, false, nil
		}
		return containerState{}, false, err
	}
	raw, err := w.exec(ctx, nil, 128*1024, "container", "inspect", name)
	if err != nil {
		return containerState{}, false, err
	}
	var states []containerState
	if json.Unmarshal(raw, &states) != nil || len(states) != 1 || states[0].ID == "" {
		return containerState{}, false, fmt.Errorf("invalid OCI inspection")
	}
	states[0].Image = imageID(states[0].Image)
	return states[0], true, nil
}

func (w *Worker) sandbox(name, runID, input, phase, network string, seconds int) []string {
	return []string{
		"--name", name, "--pull=never", "--image-volume=ignore", "--read-only", "--read-only-tmpfs=false", "--user=65532:65532",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=128", "--memory=1g", "--memory-swap=1g", "--cpus=1",
		"--ulimit=nofile=256:256", "--ulimit=fsize=4194304:4194304", "--timeout=" + strconv.Itoa(seconds), "--stop-timeout=5", "--network=" + network,
		"--http-proxy=false", "--unsetenv-all", "--env=PATH=/usr/local/bin:/usr/bin:/bin",
		"--tmpfs=/work:rw,nosuid,nodev,noexec,size=64m,mode=1777", "--tmpfs=/tmp:rw,nosuid,nodev,noexec,size=16m,mode=1777",
		"--log-driver=k8s-file", "--log-opt=max-size=1mb",
		"--label=io.lightpath.atg.worker=" + workerLabel, "--label=io.lightpath.atg.run=" + runLabel(runID),
		"--label=io.lightpath.atg.input=" + input, "--label=io.lightpath.atg.phase=" + phase,
	}
}

func (w *Worker) Ready(ctx context.Context) error {
	if err := w.allowed(); err != nil {
		return err
	}
	raw, err := w.exec(ctx, nil, 128*1024, "info", "--format=json")
	if err != nil {
		return err
	}
	var info struct {
		Host struct {
			OS       string `json:"os"`
			Security struct {
				Rootless bool `json:"rootless"`
			}
		}
	}
	if json.Unmarshal(raw, &info) != nil || info.Host.OS != "linux" || !info.Host.Security.Rootless {
		return fmt.Errorf("generation requires a rootless Linux OCI engine")
	}
	w.mu.Lock()
	target, frozen := w.image, w.imageID
	w.mu.Unlock()
	if frozen != "" {
		target = frozen
	}
	raw, err = w.exec(ctx, nil, 256*1024, "image", "inspect", target)
	if err != nil {
		return err
	}
	var images []struct {
		ID     string `json:"Id"`
		Config struct{ Labels map[string]string }
	}
	if json.Unmarshal(raw, &images) != nil || len(images) != 1 || imageID(images[0].ID) == "" || images[0].Config.Labels["io.lightpath.atg.worker"] != workerLabel || images[0].Config.Labels["io.lightpath.atg.harness"] != w.policy.Harness {
		return fmt.Errorf("generation image recipe/harness identity mismatch")
	}
	id := imageID(images[0].ID)
	if frozen != "" && id != frozen {
		return fmt.Errorf("frozen generation image identity changed")
	}
	args := append([]string{"run", "--rm"}, w.sandbox("atg-generation-probe-"+strconv.FormatInt(time.Now().UnixNano(), 36), "readiness", "readiness", "probe", "none", 30)...)
	args = append(args, id, "probe")
	raw, err = w.exec(ctx, nil, 64*1024, args...)
	if err != nil {
		return fmt.Errorf("generation OCI capability probe failed: %w", err)
	}
	var probe struct {
		SchemaVersion     string `json:"schema_version"`
		WorkerVersion     string `json:"worker_version"`
		PythonVersion     string `json:"python_version"`
		ModelCallsAllowed bool   `json:"model_calls_allowed"`
		LiveIntegration   string `json:"live_integration"`
		Harnesses         map[string]struct {
			Installed       bool    `json:"installed"`
			Version         *string `json:"version"`
			ExpectedVersion string  `json:"expected_version"`
			LiveSupported   bool    `json:"live_supported"`
			Limitation      string  `json:"limitation,omitempty"`
		} `json:"harnesses"`
	}
	if err := strictJSON(raw, &probe); err != nil || probe.SchemaVersion != "1" || probe.WorkerVersion != workerVersion || probe.PythonVersion == "" || probe.ModelCallsAllowed || probe.LiveIntegration != "not_verified" {
		return fmt.Errorf("generation worker protocol probe mismatch")
	}
	harness, ok := probe.Harnesses[w.policy.Harness]
	expected := "2.68.0"
	if w.policy.Harness == "aider" {
		expected = "0.86.2"
	}
	if !ok || !harness.Installed || harness.Version == nil || *harness.Version != expected || harness.ExpectedVersion != expected {
		return fmt.Errorf("generation harness dependency probe mismatch")
	}
	if !harness.LiveSupported {
		return fmt.Errorf("selected generation harness does not support bounded live operations")
	}
	for name, other := range probe.Harnesses {
		if name != w.policy.Harness && other.Installed {
			return fmt.Errorf("generation image contains a co-installed harness")
		}
	}
	if _, err = w.put(ctx, w.policy.raw); err != nil {
		return err
	}
	bound, _ := json.Marshal(struct {
		SchemaVersion   string          `json:"schema_version"`
		PolicyDigest    string          `json:"policy_digest"`
		Policy          json.RawMessage `json:"policy"`
		LocalImageID    string          `json:"local_image_id"`
		PromptVersion   string          `json:"prompt_version"`
		ProtocolVersion string          `json:"protocol_version"`
	}{"1", w.policy.Digest, w.policy.raw, id, PromptVersion, ProtocolVersion})
	ref, err := w.put(ctx, bound)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.imageID != "" && w.imageID != id {
		return fmt.Errorf("generation image binding changed during readiness")
	}
	w.imageID, w.policyDigest = id, ref
	return nil
}
