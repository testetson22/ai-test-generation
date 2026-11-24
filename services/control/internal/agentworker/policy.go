// Package agentworker adapts bounded generation tasks to a rootless OCI worker.
// It does not own lifecycle transitions, approval, retries, or Maven execution.
package agentworker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
)

const ProtocolVersion = "1"
const PromptVersion = "generation-stage-v4"
const workerVersion = "0.6.0"
const workerLabel = "generation-v6"
const maxTaskBytes = 2 * 1024 * 1024
const maxArchiveBytes = 32 * 1024 * 1024

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var commitPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var sourcePathPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$`)
var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

type Model struct {
	Provider string `json:"provider"`
	Endpoint string `json:"endpoint"`
	Name     string `json:"name"`
}

type Limits struct {
	TimeoutSeconds  int `json:"timeout_seconds"`
	MaxOutputBytes  int `json:"max_output_bytes"`
	MaxContextBytes int `json:"max_context_bytes"`
	MaxRequests     int `json:"max_requests"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

type Policy struct {
	SchemaVersion string   `json:"schema_version"`
	Harness       string   `json:"harness"`
	Model         Model    `json:"model"`
	ContextPaths  []string `json:"context_paths,omitempty"`
	Limits        Limits   `json:"limits"`
	NetworkPolicy string   `json:"network_policy"`
	Digest        string   `json:"-"`
	raw           []byte
}

func LoadPolicy(filename string) (Policy, error) {
	f, err := os.Open(filename)
	if err != nil {
		return Policy{}, fmt.Errorf("open generation policy: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		return Policy{}, err
	}
	return DecodePolicy(raw)
}

func DecodePolicy(raw []byte) (Policy, error) {
	var p Policy
	if len(raw) == 0 || len(raw) > 64*1024 {
		return p, fmt.Errorf("invalid generation policy size")
	}
	if err := strictJSON(raw, &p); err != nil {
		return p, err
	}
	if (p.SchemaVersion != "1" && p.SchemaVersion != "2") || (p.Harness != "pydantic_ai" && p.Harness != "aider") || p.NetworkPolicy != "public-egress-development" {
		return p, fmt.Errorf("unsupported generation policy version, harness, or network policy")
	}
	u, err := url.Parse(p.Model.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(p.Model.Endpoint, "\r\n\x00") || p.Model.Provider != "openai_compatible" || len(p.Model.Endpoint) > 2048 || !modelNamePattern.MatchString(p.Model.Name) || len(p.Model.Name) > 256 {
		return p, fmt.Errorf("model requires a credential-free HTTPS endpoint and explicit name")
	}
	l := p.Limits
	if l.TimeoutSeconds < 1 || l.TimeoutSeconds > 600 || l.MaxOutputBytes < 4096 || l.MaxOutputBytes > 1048576 || l.MaxContextBytes < 1 || l.MaxContextBytes > 1048576 || l.MaxRequests < 1 || l.MaxRequests > 8 || l.MaxOutputTokens < 1 || l.MaxOutputTokens > 32768 {
		return p, fmt.Errorf("generation limits exceed supported bounds")
	}
	if p.SchemaVersion == "1" {
		if len(p.ContextPaths) < 1 || len(p.ContextPaths) > 64 {
			return p, fmt.Errorf("legacy context requires 1..64 explicit files")
		}
		seen := map[string]bool{}
		for _, name := range p.ContextPaths {
			if !safePath(name) || seen[name] {
				return p, fmt.Errorf("legacy context paths must be unique repository-relative files")
			}
			seen[name] = true
		}
	} else if len(p.ContextPaths) != 0 {
		return p, fmt.Errorf("repository-discovery policy cannot contain explicit source files")
	}
	p.raw = bytes.Clone(raw)
	p.Digest = artifact.Digest(raw)
	return p, nil
}

func safePath(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 240 && sourcePathPattern.MatchString(name) && path.Clean(name) == name && !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") && !strings.Contains(name, "/../") && !strings.ContainsAny(name, "\\\x00\r\n") && utf8.ValidString(name)
}

func requiredJSON(raw []byte, keys ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}

// Reject duplicate keys, case aliases, unknown fields and trailing JSON before
// decoding. Different parsers must not disagree on a digest-bound task.
func strictJSON(raw []byte, target any) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("invalid JSON UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func() (any, error)
	value = func() (any, error) {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				m := map[string]any{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return nil, err
					}
					k, ok := key.(string)
					if !ok {
						return nil, fmt.Errorf("invalid JSON key")
					}
					if _, exists := m[k]; exists {
						return nil, fmt.Errorf("duplicate JSON key")
					}
					v, err := value()
					if err != nil {
						return nil, err
					}
					m[k] = v
				}
				_, err := d.Token()
				return m, err
			case '[':
				var a []any
				for d.More() {
					v, err := value()
					if err != nil {
						return nil, err
					}
					a = append(a, v)
				}
				_, err := d.Token()
				return a, err
			default:
				return nil, fmt.Errorf("unexpected JSON delimiter")
			}
		}
		return t, nil
	}
	v, err := value()
	if err != nil {
		return fmt.Errorf("invalid JSON document")
	}
	if _, err = d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON document")
	}
	var check func(any, reflect.Type) error
	check = func(v any, typ reflect.Type) error {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ == reflect.TypeFor[json.RawMessage]() {
			return nil
		}
		switch typ.Kind() {
		case reflect.Struct:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			fields := map[string]reflect.Type{}
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				key := strings.Split(f.Tag.Get("json"), ",")[0]
				if f.IsExported() && key != "-" {
					if key == "" {
						key = f.Name
					}
					fields[key] = f.Type
				}
			}
			for key, item := range m {
				ft, ok := fields[key]
				if !ok {
					return fmt.Errorf("unknown JSON field")
				}
				if err := check(item, ft); err != nil {
					return err
				}
			}
		case reflect.Slice:
			if a, ok := v.([]any); ok {
				for _, item := range a {
					if err := check(item, typ.Elem()); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := check(v, reflect.TypeOf(target).Elem()); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON fields")
	}
	return nil
}
