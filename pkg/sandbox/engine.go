// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// engineHost is the URL host every engine request carries. The real path to
// the Docker daemon is the executor proxy, which forwards req.URL.RequestURI()
// (path + query) and ignores the host, so this value never leaves the process.
const engineHost = "docker"

// Engine is a minimal Docker Engine API client over an http.RoundTripper. In
// production the transport is execpool.NewProxyTransport(pool, launcherID,
// DockerProxyName); in tests it is an httptest.Server's transport. Every path
// is unversioned.
//
// The Engine is a thin, stateless shell: it holds no pool lock and does no
// retrying. Callers must not hold a pool or controller lock across a call,
// since each request blocks on the network.
type Engine struct{ hc *http.Client }

// NewEngine wraps rt in an Engine. A nil rt falls back to http.DefaultTransport.
func NewEngine(rt http.RoundTripper) *Engine {
	return &Engine{hc: &http.Client{Transport: rt}}
}

// ContainerInfo is the subset of InspectContainer's response the launcher
// needs: the daemon-verified id, whether the container is running, and the
// container's labels.
type ContainerInfo struct {
	ID      string
	Running bool
	Labels  map[string]string
}

// ContainerSummary is one entry of ListContainers.
type ContainerSummary struct {
	ID     string
	State  string
	Labels map[string]string
}

// ImageExists reports whether ref is present locally: a 200 is true, a 404 is
// false, and any other status is an error.
func (e *Engine) ImageExists(ctx context.Context, ref string) (bool, error) {
	req, err := e.newRequest(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil)
	if err != nil {
		return false, err
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("sandbox: inspect image %s: %w", ref, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, statusError("inspect image "+ref, resp)
	}
}

// ImageID returns the content-addressed image ID (sha256:…) a ref currently
// resolves to locally. A 404 is an error: the caller decides whether to pull.
func (e *Engine) ImageID(ctx context.Context, ref string) (string, error) {
	req, err := e.newRequest(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil)
	if err != nil {
		return "", err
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("sandbox: inspect image %s: %w", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("inspect image "+ref, resp)
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("sandbox: inspect image %s: decoding response: %w", ref, err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("sandbox: inspect image %s: docker returned no image id", ref)
	}
	return out.ID, nil
}

// PullImage pulls ref, reading the whole progress stream. Docker reports a
// failed pull with status 200 and an {"error": …} object mid-stream, so a
// non-empty error key is a failure even on a 2xx.
//
// The reference splits at the LAST ':' that appears after the last '/', with
// the tag defaulting to "latest"; a digest reference (containing '@') goes
// whole into fromImage with no tag.
func (e *Engine) PullImage(ctx context.Context, ref string) error {
	name, tag := splitImageRef(ref)
	q := url.Values{}
	q.Set("fromImage", name)
	if tag != "" {
		q.Set("tag", tag)
	}
	req, err := e.newRequest(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: pull image %s: %w", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError("pull image "+ref, resp)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("sandbox: pull image %s: reading progress: %w", ref, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("sandbox: pull image %s: %s", ref, msg.Error)
		}
	}
}

// CreateContainer creates a container from body, naming it name, and returns
// the container id Docker assigned. A 201 is expected; anything else is an
// error.
func (e *Engine) CreateContainer(ctx context.Context, name string, body []byte) (string, error) {
	q := url.Values{}
	q.Set("name", name)
	req, err := e.newRequest(ctx, http.MethodPost, "/containers/create", q, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("sandbox: create container %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", statusError("create container "+name, resp)
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("sandbox: create container %s: decoding response: %w", name, err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("sandbox: create container %s: docker returned no container id", name)
	}
	return out.ID, nil
}

// StartContainer starts a container. A 204 (started now) and a 304 (already
// running) are both success.
func (e *Engine) StartContainer(ctx context.Context, id string) error {
	req, err := e.newRequest(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
	if err != nil {
		return err
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: start container %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	return statusError("start container "+id, resp)
}

// StopContainer stops a container, waiting timeoutSeconds before killing it. A
// 204 (stopped now) and a 304 (already stopped) are success, and a 404 (gone)
// is treated as stopped — the caller asked for it to be gone.
func (e *Engine) StopContainer(ctx context.Context, id string, timeoutSeconds int) error {
	q := url.Values{}
	q.Set("t", strconv.Itoa(timeoutSeconds))
	req, err := e.newRequest(ctx, http.MethodPost, "/containers/"+id+"/stop", q, nil)
	if err != nil {
		return err
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: stop container %s: %w", id, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified, http.StatusNotFound:
		return nil
	default:
		return statusError("stop container "+id, resp)
	}
}

// RemoveContainer removes a container. force also removes a running one,
// volumes also removes its anonymous volumes. A 204 is success and a 404 (gone)
// is treated as removed.
func (e *Engine) RemoveContainer(ctx context.Context, id string, force, volumes bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	if volumes {
		q.Set("v", "1")
	}
	req, err := e.newRequest(ctx, http.MethodDelete, "/containers/"+id, q, nil)
	if err != nil {
		return err
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: remove container %s: %w", id, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return statusError("remove container "+id, resp)
	}
}

// InspectContainer returns the container's info. A 404 reports (zero, false,
// nil): the container is gone, which is not an error. Any other non-200 status
// is an error.
func (e *Engine) InspectContainer(ctx context.Context, id string) (ContainerInfo, bool, error) {
	req, err := e.newRequest(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil)
	if err != nil {
		return ContainerInfo{}, false, err
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return ContainerInfo{}, false, fmt.Errorf("sandbox: inspect container %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ContainerInfo{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return ContainerInfo{}, false, statusError("inspect container "+id, resp)
	}
	var raw struct {
		ID    string `json:"Id"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return ContainerInfo{}, false, fmt.Errorf("sandbox: inspect container %s: decoding response: %w", id, err)
	}
	return ContainerInfo{ID: raw.ID, Running: raw.State.Running, Labels: raw.Config.Labels}, true, nil
}

// ListContainers lists every container (running or not) carrying the label
// labelKey.
func (e *Engine) ListContainers(ctx context.Context, labelKey string) ([]ContainerSummary, error) {
	filters, err := json.Marshal(map[string][]string{"label": {labelKey}})
	if err != nil {
		return nil, fmt.Errorf("sandbox: list containers: encoding filters: %w", err)
	}
	q := url.Values{}
	q.Set("all", "1")
	q.Set("filters", string(filters))
	req, err := e.newRequest(ctx, http.MethodGet, "/containers/json", q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list containers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError("list containers", resp)
	}
	var raw []struct {
		ID     string            `json:"Id"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("sandbox: list containers: decoding response: %w", err)
	}
	out := make([]ContainerSummary, 0, len(raw))
	for _, c := range raw {
		out = append(out, ContainerSummary{ID: c.ID, State: c.State, Labels: c.Labels})
	}
	return out, nil
}

// newRequest builds a request against the engine host. A nil q omits the query
// string entirely.
func (e *Engine) newRequest(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Request, error) {
	u := &url.URL{Scheme: "http", Host: engineHost, Path: path}
	if q != nil {
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("sandbox: building %s %s: %w", method, path, err)
	}
	return req, nil
}

// statusError turns a non-success Docker response into an error carrying the
// status and, when the body is a JSON object, its "message" field.
func statusError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) == nil && m.Message != "" {
		return fmt.Errorf("sandbox: %s: docker returned %s: %s", op, resp.Status, m.Message)
	}
	return fmt.Errorf("sandbox: %s: docker returned %s", op, resp.Status)
}

// splitImageRef splits an image reference into a fromImage name and a tag. A
// digest reference (containing '@') is returned whole with no tag; otherwise
// the split is at the LAST ':' that appears after the last '/', with the tag
// defaulting to "latest".
func splitImageRef(ref string) (name, tag string) {
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, "latest"
}
