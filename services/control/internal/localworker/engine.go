package localworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type engineError struct {
	code  int
	cause error
}

func (e *engineError) Error() string {
	return fmt.Sprintf("OCI engine command failed (exit %d): %v", e.code, e.cause)
}
func (e *engineError) Unwrap() error { return e.cause }

type runner func(context.Context, []byte, int64, ...string) ([]byte, error)

var imageIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func canonicalImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if !imageIDPattern.MatchString(id) {
		return ""
	}
	return "sha256:" + id
}

type boundedWriter struct {
	bytes.Buffer
	limit  int64
	cancel context.CancelFunc
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.limit {
		b.cancel()
		return 0, fmt.Errorf("OCI output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func engineRunner(engine string) runner {
	return func(ctx context.Context, input []byte, limit int64, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, engine, args...)
		// Only engine transport needs host configuration. No host environment is
		// passed to a worker; --unsetenv-all also clears image ENV on create.
		for _, key := range []string{"PATH", "HOME", "XDG_RUNTIME_DIR", "CONTAINER_HOST", "CONTAINER_CONNECTION", "SSH_AUTH_SOCK"} {
			if v, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+v)
			}
		}
		cmd.Stdin = bytes.NewReader(input)
		out := &boundedWriter{limit: limit, cancel: cancel}
		stderr := &boundedWriter{limit: 64 * 1024, cancel: cancel}
		cmd.Stdout, cmd.Stderr = out, stderr
		if err := cmd.Run(); err != nil {
			code := -1
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			}
			diagnostic := strings.TrimSpace(stderr.String())
			if len(diagnostic) > 4096 {
				diagnostic = diagnostic[:4096]
			}
			return nil, &engineError{code: code, cause: fmt.Errorf("%w: %s", err, diagnostic)}
		}
		return out.Bytes(), nil
	}
}

type containerState struct {
	ID      string `json:"Id"`
	Image   string
	Created time.Time
	Config  struct{ Labels map[string]string }
	State   struct {
		Status    string
		Running   bool
		ExitCode  int
		StartedAt time.Time
	}
}

func (w *Worker) inspect(ctx context.Context, name string) (containerState, bool, error) {
	_, err := w.exec(ctx, nil, 1024, "container", "exists", name)
	if err != nil {
		var e *engineError
		if errors.As(err, &e) && e.code == 1 {
			return containerState{}, false, nil
		}
		return containerState{}, false, err
	}
	raw, err := w.exec(ctx, nil, 128*1024, "container", "inspect", name)
	if err != nil {
		return containerState{}, false, err
	}
	var states []containerState
	if err = json.Unmarshal(raw, &states); err != nil || len(states) != 1 || states[0].ID == "" {
		return containerState{}, false, fmt.Errorf("invalid OCI inspection")
	}
	states[0].Image = canonicalImageID(states[0].Image)
	return states[0], true, nil
}

func (w *Worker) sandboxArgs(name, run, digest, network string, timeout int) []string {
	return []string{
		"--name", name, "--pull=never", "--image-volume=ignore",
		"--read-only", "--read-only-tmpfs=false", "--user=65532:65532",
		"--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--pids-limit=256", "--memory=3g", "--memory-swap=3g", "--cpus=2",
		"--ulimit=nofile=1024:1024", "--ulimit=fsize=536870912:536870912",
		"--timeout=" + strconv.Itoa(timeout), "--stop-timeout=5", "--network=" + network,
		"--http-proxy=false", "--unsetenv-all", "--env=PATH=/usr/local/bin:/usr/bin:/bin",
		"--tmpfs=/work:rw,nosuid,nodev,size=1536m,mode=1777",
		"--tmpfs=/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777",
		"--log-driver=k8s-file", "--log-opt=max-size=1mb",
		"--label=io.lightpath.atg.worker=" + workerLabel,
		"--label=io.lightpath.atg.run=" + run,
		"--label=io.lightpath.atg.input=" + digest,
	}
}

func (w *Worker) Ready(ctx context.Context) error {
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
		return fmt.Errorf("worker requires a rootless Linux OCI engine")
	}
	w.mu.Lock()
	target := w.image
	frozenID := w.imageID
	w.mu.Unlock()
	if frozenID != "" {
		target = frozenID
	}
	raw, err = w.exec(ctx, nil, 256*1024, "image", "inspect", target)
	if err != nil {
		return err
	}
	var images []struct {
		ID     string `json:"Id"`
		Config struct{ Labels map[string]string }
	}
	if json.Unmarshal(raw, &images) != nil || len(images) != 1 || images[0].Config.Labels["io.lightpath.atg.worker"] != workerLabel || canonicalImageID(images[0].ID) == "" {
		return fmt.Errorf("worker image identity or recipe label is invalid")
	}
	imageID := canonicalImageID(images[0].ID)
	if frozenID != "" && imageID != frozenID {
		return fmt.Errorf("OCI engine changed a frozen image identity")
	}
	name := "atg-probe-" + strings.TrimPrefix(digestJSON(time.Now().UnixNano()), "sha256:")[:24]
	args := append([]string{"run", "--rm"}, w.sandboxArgs(name, "readiness", "readiness", "none", 30)...)
	args = append(args, imageID, "probe")
	_, err = w.exec(ctx, nil, 64*1024, args...)
	if err != nil {
		return fmt.Errorf("OCI resource/read-only/nonroot capability probe: %w", err)
	}
	if _, err = w.put(ctx, w.profile.raw); err != nil {
		return fmt.Errorf("retain base worker profile: %w", err)
	}
	ref, err := w.put(ctx, boundProfile(w.profile, imageID))
	if err != nil {
		return fmt.Errorf("retain bound worker profile: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.imageID != "" && w.imageID != imageID {
		return fmt.Errorf("worker image binding changed during readiness")
	}
	w.imageID = imageID
	w.boundDigest.Store(ref)
	return nil
}

func (w *Worker) stop(ctx context.Context, id string) error {
	_, stopErr := w.exec(ctx, nil, 4096, "stop", "--time=5", id)
	state, exists, err := w.inspect(ctx, id)
	if err != nil {
		return err
	}
	if !exists || state.State.Running {
		return fmt.Errorf("worker termination is not confirmed: %w", stopErr)
	}
	return nil
}

var _ io.Writer = (*boundedWriter)(nil)
