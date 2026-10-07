package integration_test

// sandbox_fakedocker_test.go holds an in-process fake Docker Engine for the
// sandbox integration tests. It listens on a unix socket (the launcher under
// test is given `--proxy docker=unix://<that socket>`, so every Engine call the
// daemon makes reaches it through the real executor proxy, the real create-body
// guard and the real relay).
//
// It implements exactly the Engine client's calls (pkg/sandbox/engine.go):
//
//	GET    /images/{ref}/json
//	POST   /images/create
//	POST   /containers/create
//	POST   /containers/{id}/start
//	POST   /containers/{id}/stop
//	DELETE /containers/{id}
//	GET    /containers/json
//	GET    /containers/{id}/json
//
// On create it records the body. On start it "plays the container": it launches
// the real `rafiki executor serve --connect-socket <host path of the relay
// mount>/daemon.sock` subprocess with the create body's Env (which carries
// RAFIKI_EXECUTOR_CREDENTIAL), so the real relay, the real credential path and a
// real executor are all exercised. On DELETE (and stop) it kills that process.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/sandbox"
)

// dockerMount mirrors one entry of the create body's HostConfig.Mounts.
type dockerMount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

// dockerCreateBody is the create body decoded into the fields the fake needs.
type dockerCreateBody struct {
	Image      string            `json:"Image"`
	Entrypoint []string          `json:"Entrypoint"`
	Cmd        []string          `json:"Cmd"`
	WorkingDir string            `json:"WorkingDir"`
	Env        []string          `json:"Env"`
	Labels     map[string]string `json:"Labels"`
	HostConfig struct {
		Mounts         []dockerMount `json:"Mounts"`
		NetworkMode    string        `json:"NetworkMode"`
		ReadonlyRootfs bool          `json:"ReadonlyRootfs"`
		Memory         int64         `json:"Memory"`
		NanoCpus       int64         `json:"NanoCpus"`
		PidsLimit      int64         `json:"PidsLimit"`
		RestartPolicy  struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

// dockerCreate is one recorded create.
type dockerCreate struct {
	ID   string
	Name string
	Body dockerCreateBody
	Raw  []byte
}

// fakeContainer is one container the fake is "running".
type fakeContainer struct {
	id     string
	name   string
	body   dockerCreateBody
	state  string // "created" | "running" | "exited"
	proc   *exec.Cmd
	gone   bool // dropped from the fake's view without a DELETE (a lost container)
	orphan bool // inserted by the test, never created through the daemon
}

// fakeDocker is the in-process Docker Engine.
type fakeDocker struct {
	t          *testing.T
	socketPath string
	ln         net.Listener
	srv        *http.Server
	homeDir    string // HOME for the launched "container" executor

	mu          sync.Mutex
	imageExists bool
	creates     []dockerCreate
	containers  map[string]*fakeContainer
	removed     []string
	nextID      int
	createFail  bool
}

// newFakeDocker starts the fake on a unix socket under a temp dir.
func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	dir, err := os.MkdirTemp(socketTempBase(), "rafiki-fakedocker-")
	assert.NewAborting(t).NoError(err, "mkdirtemp fake docker")
	socketPath := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", socketPath)
	assert.NewAborting(t).NoError(err, "listen fake docker")

	fd := &fakeDocker{
		t:           t,
		socketPath:  socketPath,
		ln:          ln,
		homeDir:     filepath.Join(dir, "home"),
		imageExists: true,
		containers:  map[string]*fakeContainer{},
	}
	_ = os.MkdirAll(fd.homeDir, 0o700)
	fd.srv = &http.Server{Handler: fd}
	go func() { _ = fd.srv.Serve(ln) }()
	t.Cleanup(func() {
		fd.killAll()
		_ = fd.srv.Close()
		os.RemoveAll(dir)
	})
	return fd
}

func socketTempBase() string {
	// A unix socket path must stay under the ~104 byte sun_path limit; on macOS
	// the default temp dir resolves through /private/var/folders/…, so use /tmp.
	if st, err := os.Stat("/tmp"); err == nil && st.IsDir() {
		return "/tmp"
	}
	return ""
}

// socketPathOnHost returns the path the fake listens on.
func (d *fakeDocker) socketPathOnHost() string { return d.socketPath }

// creates returns a copy of the recorded create bodies.
func (d *fakeDocker) recordedCreates() []dockerCreate {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]dockerCreate, len(d.creates))
	copy(out, d.creates)
	return out
}

// createCount returns how many container creates the fake has seen.
func (d *fakeDocker) createCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.creates)
}

// removedIDs returns the container ids the fake was asked to DELETE.
func (d *fakeDocker) removedIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.removed))
	copy(out, d.removed)
	return out
}

// hasRemoved reports whether a DELETE arrived for id.
func (d *fakeDocker) hasRemoved(id string) bool {
	for _, r := range d.removedIDs() {
		if r == id {
			return true
		}
	}
	return false
}

// dropContainer simulates the container vanishing without a DELETE: the row the
// daemon holds no longer has a container.
func (d *fakeDocker) dropContainer(id string) {
	d.mu.Lock()
	c, ok := d.containers[id]
	var proc *exec.Cmd
	if ok {
		c.gone = true
		proc = c.proc
	}
	d.mu.Unlock()
	// The container is gone, so its executor process is too: kill it so the
	// daemon sees the sandbox's executor disconnect.
	if proc != nil && proc.Process != nil {
		_ = proc.Process.Kill()
	}
}

// addOrphan inserts a container the daemon has no row for, labelled as sandbox
// rowID, so the reaper's orphan arm must remove it.
func (d *fakeDocker) addOrphan(id, rowID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.containers[id] = &fakeContainer{
		id:     id,
		name:   "orphan-" + id,
		state:  "running",
		orphan: true,
		body: dockerCreateBody{Labels: map[string]string{
			sandbox.DockerLabelSandbox: rowID,
		}},
	}
}

func (d *fakeDocker) killAll() {
	d.mu.Lock()
	procs := make([]*exec.Cmd, 0, len(d.containers))
	for _, c := range d.containers {
		if c.proc != nil {
			procs = append(procs, c.proc)
		}
	}
	d.mu.Unlock()
	for _, p := range procs {
		_ = p.Process.Kill()
		_, _ = p.Process.Wait()
	}
}

// ServeHTTP implements the Engine API subset.
func (d *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
		d.mu.Lock()
		exists := d.imageExists
		d.mu.Unlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"Id":"sha256:fake"}`)

	case r.Method == http.MethodPost && p == "/images/create":
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"done"}`+"\n")

	case r.Method == http.MethodPost && p == "/containers/create":
		d.handleCreate(w, r, q.Get("name"))

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/start"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/start")
		if err := d.handleStart(id); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"message":%q}`, err.Error()))
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/stop"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/stop")
		d.stopProcess(id)
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/containers/"):
		id := strings.TrimPrefix(p, "/containers/")
		d.mu.Lock()
		_, ok := d.containers[id]
		if ok {
			d.removed = append(d.removed, id)
			delete(d.containers, id)
		}
		d.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		d.stopProcess(id)
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && p == "/containers/json":
		d.mu.Lock()
		type row struct {
			ID     string            `json:"Id"`
			State  string            `json:"State"`
			Labels map[string]string `json:"Labels"`
		}
		var out []row
		for _, c := range d.containers {
			if c.gone {
				continue
			}
			out = append(out, row{ID: c.id, State: c.state, Labels: c.body.Labels})
		}
		d.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
		d.mu.Lock()
		c, ok := d.containers[id]
		d.mu.Unlock()
		if !ok || c.gone {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id":     c.id,
			"State":  map[string]any{"Running": c.state == "running"},
			"Config": map[string]any{"Labels": c.body.Labels},
		})

	default:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"fake docker: unhandled route"}`)
	}
}

func (d *fakeDocker) handleCreate(w http.ResponseWriter, r *http.Request, name string) {
	body, _ := io.ReadAll(r.Body)
	var parsed dockerCreateBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"bad body"}`)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.createFail {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"create refused"}`)
		return
	}
	d.nextID++
	id := fmt.Sprintf("ctr-%d", d.nextID)
	d.creates = append(d.creates, dockerCreate{ID: id, Name: name, Body: parsed, Raw: body})
	d.containers[id] = &fakeContainer{id: id, name: name, body: parsed, state: "created"}
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, fmt.Sprintf(`{"Id":%q,"Warnings":[]}`, id))
}

// relayHostDirFromBody returns the host directory the container's relay mount
// binds from (the launcher's --relay-dir).
func relayHostDirFromBody(b dockerCreateBody) string {
	for _, m := range b.HostConfig.Mounts {
		if m.Target == sandbox.ContainerRelayDir && m.Type == "bind" {
			return m.Source
		}
	}
	return ""
}

func (d *fakeDocker) handleStart(id string) error {
	d.mu.Lock()
	c, ok := d.containers[id]
	if !ok || c.gone {
		d.mu.Unlock()
		return fmt.Errorf("no container %s", id)
	}
	body := c.body
	d.mu.Unlock()

	relayHostDir := relayHostDirFromBody(body)
	if relayHostDir == "" {
		return fmt.Errorf("container %s has no relay bind mount", id)
	}
	relaySocket := filepath.Join(relayHostDir, sandboxrelaySocketName())

	// Map the container's argv to a real subprocess: the entrypoint plus Cmd
	// name `rafiki executor serve --connect-socket /run/rafiki-relay/daemon.sock`,
	// and the container path of the relay socket maps to its host path.
	args := append([]string{}, body.Cmd...)
	for i, a := range args {
		if a == sandbox.ContainerRelayDir+"/"+sandboxrelaySocketName() {
			args[i] = relaySocket
		}
	}

	cmd := exec.Command(cliBinary(), args...)
	env := append([]string{}, body.Env...)
	env = append(env, "PATH="+os.Getenv("PATH"))
	env = append(env, "HOME="+d.homeDir)
	cmd.Env = env
	cmd.Dir = d.homeDir

	d.mu.Lock()
	c.state = "running"
	d.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start container %s: %w", id, err)
	}
	d.mu.Lock()
	c.proc = cmd
	d.mu.Unlock()
	go func() { _ = cmd.Wait() }()
	return nil
}

func (d *fakeDocker) stopProcess(id string) {
	d.mu.Lock()
	c, ok := d.containers[id]
	var proc *exec.Cmd
	if ok {
		c.state = "exited"
		proc = c.proc
	}
	d.mu.Unlock()
	if proc != nil && proc.Process != nil {
		_ = proc.Process.Kill()
	}
}

// sandboxrelaySocketName mirrors sandboxrelay.SocketName without importing the
// package (which cmd/rafiki owns); it is the fixed name the relay serves.
func sandboxrelaySocketName() string { return "daemon.sock" }

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
