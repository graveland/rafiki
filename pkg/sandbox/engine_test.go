// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/multigres/testkit/assert"
)

// recordedRequest is one request the fake engine saw.
type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	RawURI string
	Body   []byte
}

// engineRecorder captures every request an Engine sends to the fake server.
type engineRecorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (r *engineRecorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recordedRequest{
		Method: req.Method,
		Path:   req.URL.Path,
		Query:  req.URL.Query(),
		RawURI: req.URL.RequestURI(),
		Body:   body,
	})
}

func (r *engineRecorder) last(t *testing.T) recordedRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		t.Fatalf("engine saw no request")
	}
	return r.reqs[len(r.reqs)-1]
}

// newTestEngine returns an Engine whose transport is a fake server running
// respond, plus the recorder of the requests it received.
func newTestEngine(t *testing.T, respond http.HandlerFunc) (*Engine, *engineRecorder) {
	t.Helper()
	rec := &engineRecorder{}
	// NewTestServer serves on an in-memory network whose transport dials the
	// listener regardless of the URL host, so requests carrying the fixed
	// engine host "docker" reach it. NewServer's transport dials the host in
	// the URL, which is not "docker".
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)
		respond(w, req)
	}))
	return NewEngine(srv.Client().Transport), rec
}

func TestEngineImageExists(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	c := assert.NewAborting(t)
	ok, err := e.ImageExists(context.Background(), "nginx:latest")
	c.NoError(err, "ImageExists")
	c.True(ok, "200 means present")
	got := rec.last(t)
	c.Eq("GET", got.Method, "method")
	c.Eq("/images/nginx:latest/json", got.Path, "path")
	c.Eq("", got.RawURI[len("/images/nginx:latest/json"):], "no query")
}

func TestEngineImageExistsNotFound(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c := assert.NewAborting(t)
	ok, err := e.ImageExists(context.Background(), "nginx:latest")
	c.NoError(err, "404 is not an error")
	c.False(ok, "404 means absent")
	c.Eq("/images/nginx:latest/json", rec.last(t).Path, "path")
}

func TestEngineImageExistsError(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})
	_, err := e.ImageExists(context.Background(), "nginx")
	assert.NewAborting(t).Error(err, "500 is an error")
}

func TestEngineImageIDReturnsID(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Id":"sha256:abc"}`))
	})
	c := assert.NewAborting(t)
	id, err := e.ImageID(context.Background(), "nginx:latest")
	c.NoError(err, "ImageID")
	c.Eq("sha256:abc", id, "id")
	got := rec.last(t)
	c.Eq("GET", got.Method, "method")
	c.Eq("/images/nginx:latest/json", got.Path, "path")
}

func TestEngineImageIDNotFoundIsError(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := e.ImageID(context.Background(), "nginx:latest")
	c := assert.NewAborting(t)
	c.Error(err, "a 404 is an error: the caller decides whether to pull")
	c.StrContains(err.Error(), "404", "error carries the status")
}

func TestEngineImageIDEmptyIDIsError(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Id":""}`))
	})
	_, err := e.ImageID(context.Background(), "nginx:latest")
	assert.NewAborting(t).Error(err, "an empty id is an error")
}

func TestEnginePullImage(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"Pulling from library/nginx"}` + "\n" + `{"status":"Download complete"}` + "\n"))
	})
	c := assert.NewAborting(t)
	c.NoError(e.PullImage(context.Background(), "nginx"), "PullImage")
	got := rec.last(t)
	c.Eq("POST", got.Method, "method")
	c.Eq("/images/create", got.Path, "path")
	c.Eq("nginx", got.Query.Get("fromImage"), "fromImage")
	c.Eq("latest", got.Query.Get("tag"), "tag defaults to latest")
}

func TestEnginePullImageTag(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	c := assert.NewAborting(t)
	c.NoError(e.PullImage(context.Background(), "nginx:1.2"), "PullImage")
	got := rec.last(t)
	c.Eq("nginx", got.Query.Get("fromImage"), "fromImage")
	c.Eq("1.2", got.Query.Get("tag"), "tag")
}

func TestEnginePullImageDigest(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	c := assert.NewAborting(t)
	c.NoError(e.PullImage(context.Background(), "a@sha256:abc"), "PullImage")
	got := rec.last(t)
	c.Eq("a@sha256:abc", got.Query.Get("fromImage"), "digest goes whole into fromImage")
	c.Eq("", got.Query.Get("tag"), "no tag for a digest")
}

// TestEnginePullFailureInStreamIsAnError pins that a failed pull — a 200 with
// an {"error": …} object mid-stream — is an error.
func TestEnginePullFailureInStreamIsAnError(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"error":"manifest unknown"}` + "\n"))
	})
	err := e.PullImage(context.Background(), "nginx")
	c := assert.NewAborting(t)
	c.Error(err, "an error object in the stream is a failure")
	c.StrContains(err.Error(), "manifest unknown", "error carries docker's message")
}

// TestEngineSplitImageRef is the reference split table.
func TestEngineSplitImageRef(t *testing.T) {
	tests := []struct {
		ref      string
		wantName string
		wantTag  string
	}{
		{"nginx", "nginx", "latest"},
		{"nginx:1.2", "nginx", "1.2"},
		{"localhost:5000/a/b", "localhost:5000/a/b", "latest"},
		{"localhost:5000/a/b:v1", "localhost:5000/a/b", "v1"},
		{"a@sha256:abc", "a@sha256:abc", ""},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			name, tag := splitImageRef(tt.ref)
			c := assert.NewAborting(t)
			c.Eq(tt.wantName, name, "name")
			c.Eq(tt.wantTag, tag, "tag")
		})
	}
}

func TestEngineCreateContainer(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"deadbeef","Warnings":[]}`))
	})
	c := assert.NewAborting(t)
	id, err := e.CreateContainer(context.Background(), "rafiki-sbx-1", []byte(`{"Image":"x"}`))
	c.NoError(err, "CreateContainer")
	c.Eq("deadbeef", id, "id")
	got := rec.last(t)
	c.Eq("POST", got.Method, "method")
	c.Eq("/containers/create", got.Path, "path")
	c.Eq("rafiki-sbx-1", got.Query.Get("name"), "name")
	c.EqDeep([]byte(`{"Image":"x"}`), got.Body, "body forwarded")
}

func TestEngineCreateContainerError(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"no such image"}`))
	})
	_, err := e.CreateContainer(context.Background(), "n", nil)
	c := assert.NewAborting(t)
	c.Error(err, "500 is an error")
	c.StrContains(err.Error(), "no such image", "error carries docker's message")
	c.StrContains(err.Error(), "500", "error carries the status")
}

func TestEngineStartContainer(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusNotModified} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			})
			c := assert.NewAborting(t)
			c.NoError(e.StartContainer(context.Background(), "cid"), "start is success on %d", code)
			got := rec.last(t)
			c.Eq("POST", got.Method, "method")
			c.Eq("/containers/cid/start", got.Path, "path")
		})
	}
	t.Run("error", func(t *testing.T) {
		e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		assert.NewAborting(t).Error(e.StartContainer(context.Background(), "cid"), "500 is an error")
	})
}

func TestEngineStopContainer(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := assert.NewAborting(t)
	c.NoError(e.StopContainer(context.Background(), "cid", 5), "stop")
	got := rec.last(t)
	c.Eq("POST", got.Method, "method")
	c.Eq("/containers/cid/stop", got.Path, "path")
	c.Eq("5", got.Query.Get("t"), "t")
}

func TestEngineStopContainerAlreadyStoppedOrGone(t *testing.T) {
	for _, code := range []int{http.StatusNotModified, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			})
			c := assert.NewAborting(t)
			c.NoError(e.StopContainer(context.Background(), "cid", 5), "%d is success (already stopped / gone)", code)
		})
	}
	t.Run("error", func(t *testing.T) {
		e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		assert.NewAborting(t).Error(e.StopContainer(context.Background(), "cid", 5), "500 is an error")
	})
}

func TestEngineRemoveContainer(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := assert.NewAborting(t)
	c.NoError(e.RemoveContainer(context.Background(), "cid", true, true), "remove")
	got := rec.last(t)
	c.Eq("DELETE", got.Method, "method")
	c.Eq("/containers/cid", got.Path, "path")
	c.Eq("1", got.Query.Get("force"), "force")
	c.Eq("1", got.Query.Get("v"), "v")
}

func TestEngineRemoveContainerGone(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	assert.NewAborting(t).NoError(e.RemoveContainer(context.Background(), "cid", true, false), "404 is success (gone)")
}

func TestEngineRemoveContainerError(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	assert.NewAborting(t).Error(e.RemoveContainer(context.Background(), "cid", false, false), "500 is an error")
}

func TestEngineInspectContainer(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Id":"cid","State":{"Running":true},"Config":{"Labels":{"rafiki.sandbox":"s1"}}}`))
	})
	c := assert.NewAborting(t)
	info, found, err := e.InspectContainer(context.Background(), "cid")
	c.NoError(err, "InspectContainer")
	c.True(found, "200 means found")
	c.Eq("cid", info.ID, "id")
	c.True(info.Running, "running")
	c.Eq("s1", info.Labels[DockerLabelSandbox], "labels")
	got := rec.last(t)
	c.Eq("GET", got.Method, "method")
	c.Eq("/containers/cid/json", got.Path, "path")
}

func TestEngineInspectContainerNotFound(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, found, err := e.InspectContainer(context.Background(), "cid")
	c := assert.NewAborting(t)
	c.NoError(err, "404 is not an error")
	c.False(found, "404 means not found")
}

func TestEngineInspectContainerError(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, _, err := e.InspectContainer(context.Background(), "cid")
	assert.NewAborting(t).Error(err, "500 is an error")
}

func TestEngineListContainers(t *testing.T) {
	e, rec := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"Id":"a","State":"running","Labels":{"rafiki.sandbox":"s1"}},{"Id":"b","State":"exited"}]`))
	})
	c := assert.NewAborting(t)
	got, err := e.ListContainers(context.Background(), DockerLabelSandbox)
	c.NoError(err, "ListContainers")
	c.Len(got, 2, "two containers")
	c.Eq("a", got[0].ID, "first id")
	c.Eq("running", got[0].State, "first state")
	c.Eq("s1", got[0].Labels[DockerLabelSandbox], "first label")
	c.Eq("b", got[1].ID, "second id")

	req := rec.last(t)
	c.Eq("GET", req.Method, "method")
	c.Eq("/containers/json", req.Path, "path")
	c.Eq("1", req.Query.Get("all"), "all")
	var filters map[string][]string
	c.NoError(json.Unmarshal([]byte(req.Query.Get("filters")), &filters), "filters is JSON")
	c.EqDeep([]string{DockerLabelSandbox}, filters["label"], "filters.label")
}

func TestEngineListContainersError(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := e.ListContainers(context.Background(), DockerLabelSandbox)
	assert.NewAborting(t).Error(err, "500 is an error")
}

// TestEngineNon2xxCarriesMessage pins that a non-JSON body still yields an
// error, just without the message.
func TestEngineNon2xxCarriesStatus(t *testing.T) {
	e, _ := newTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("not json"))
	})
	err := e.StartContainer(context.Background(), "cid")
	c := assert.NewAborting(t)
	c.Error(err, "502 is an error")
	c.StrContains(err.Error(), "502", "error carries the status")
}
