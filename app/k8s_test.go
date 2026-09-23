package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeKube — маленький фейк Kubernetes API: ровно те эндпоинты, которые дёргает приложение.
type fakeKube struct {
	mu       sync.Mutex
	requests []string // "METHOD path content-type body"
	pods     map[string]string
}

const (
	nsPods = "/api/v1/namespaces/demo/pods"
	nsApps = "/apis/apps/v1/namespaces/demo"
)

func newFakeKube(t *testing.T) (*fakeKube, *kubeClient) {
	t.Helper()
	f := &fakeKube{pods: map[string]string{
		"kube-demo-abc-1": `{"metadata":{"name":"kube-demo-abc-1","labels":{"app":"kube-demo"},
			"ownerReferences":[{"kind":"ReplicaSet","name":"kube-demo-abc"}]},
			"spec":{"nodeName":"worker","initContainers":[{}],"containers":[{}]},
			"status":{"phase":"Running","podIP":"10.0.0.1",
			  "initContainerStatuses":[{"state":{"terminated":{"reason":"Completed"}}}],
			  "containerStatuses":[{"ready":true,"restartCount":2,"state":{"running":{}}}]}}`,
		"kube-demo-abc-2": `{"metadata":{"name":"kube-demo-abc-2","labels":{"app":"kube-demo"},"deletionTimestamp":"2026-01-01T00:00:00Z"},
			"spec":{"containers":[{}]},"status":{"phase":"Running","containerStatuses":[{"ready":true}]}}`,
		"kube-demo-abc-3": `{"metadata":{"name":"kube-demo-abc-3","labels":{"app":"kube-demo"}},
			"spec":{"initContainers":[{}],"containers":[{}]},
			"status":{"phase":"Pending","initContainerStatuses":[{"state":{"running":{}}}],
			  "containerStatuses":[{"state":{"waiting":{"reason":"PodInitializing"}}}]}}`,
		"postgres-0": `{"metadata":{"name":"postgres-0","labels":{"app":"postgres"}},
			"spec":{"containers":[{}]},"status":{"phase":"Running",
			"containerStatuses":[{"state":{"waiting":{"reason":"CrashLoopBackOff"}},"restartCount":5}]}}`,
		"someone-else": `{"metadata":{"name":"someone-else","labels":{"app":"other"}},"spec":{},"status":{}}`,
	}}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, strings.TrimSpace(fmt.Sprintf("%s %s %s %s",
			r.Method, r.URL.Path, r.Header.Get("Content-Type"), body)))

		switch {
		case r.Method == "GET" && r.URL.Path == nsApps+"/deployments/kube-demo":
			fmt.Fprint(w, `{"metadata":{"name":"kube-demo"},"spec":{"replicas":3},
				"status":{"replicas":4,"readyReplicas":2,"updatedReplicas":1,"availableReplicas":2}}`)
		case r.Method == "GET" && r.URL.Path == nsApps+"/replicasets":
			if r.URL.Query().Get("labelSelector") != "app=kube-demo" {
				t.Errorf("unexpected labelSelector %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"items":[
				{"metadata":{"name":"kube-demo-old","creationTimestamp":"2026-01-01T00:00:00Z",
				  "annotations":{"deployment.kubernetes.io/revision":"1"}},"spec":{"replicas":1},"status":{"replicas":2,"readyReplicas":2}},
				{"metadata":{"name":"kube-demo-new","creationTimestamp":"2026-01-02T00:00:00Z",
				  "annotations":{"deployment.kubernetes.io/revision":"2"}},"spec":{"replicas":2},"status":{"replicas":1}}]}`)
		case r.Method == "GET" && r.URL.Path == nsPods:
			var items []string
			for _, p := range f.pods {
				items = append(items, p)
			}
			fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
		case strings.HasPrefix(r.URL.Path, nsPods+"/"):
			name := strings.TrimPrefix(r.URL.Path, nsPods+"/")
			pod, ok := f.pods[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"message":"pods %q not found"}`, name)
				return
			}
			if r.Method == "DELETE" {
				delete(f.pods, name)
			}
			fmt.Fprint(w, pod)
		case r.Method == "PATCH":
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"forbidden by fake"}`)
		}
	}))
	t.Cleanup(ts.Close)

	return f, &kubeClient{
		baseURL:    ts.URL,
		namespace:  "demo",
		deployment: "kube-demo",
		http:       ts.Client(),
		token:      func() (string, error) { return "test-token", nil },
	}
}

func (f *fakeKube) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func newKubeTestServer(t *testing.T) (*fakeKube, http.Handler) {
	t.Helper()
	f, kube := newFakeKube(t)
	srv, _ := newTestServer(t)
	srv.kube = kube
	return f, srv.routes()
}

func TestClusterState(t *testing.T) {
	_, h := newKubeTestServer(t)

	rec := do(t, h, "GET", "/api/k8s/state", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := decode[struct{ State clusterState }](t, rec).State

	if d := got.Deployment; d.Desired != 3 || d.Current != 4 || d.Ready != 2 || d.Updated != 1 {
		t.Errorf("deployment: %+v", d)
	}
	if len(got.ReplicaSets) != 2 || got.ReplicaSets[0].Name != "kube-demo-new" || got.ReplicaSets[0].Revision != "2" {
		t.Errorf("replicasets should be newest first: %+v", got.ReplicaSets)
	}

	want := map[string]string{ // как колонка STATUS у kubectl get pods
		"kube-demo-abc-1": "Running",
		"kube-demo-abc-2": "Terminating",
		"kube-demo-abc-3": "Init:0/1",
		"postgres-0":      "CrashLoopBackOff",
	}
	for _, p := range got.Pods {
		if w, ok := want[p.Name]; ok && p.Status != w {
			t.Errorf("%s: status %q, want %q", p.Name, p.Status, w)
		}
		if p.Name == "kube-demo-abc-1" && (p.Ready != "1/1" || p.Restarts != 2 || p.Owner != "ReplicaSet/kube-demo-abc") {
			t.Errorf("pod view: %+v", p)
		}
	}
}

func TestScale(t *testing.T) {
	f, h := newKubeTestServer(t)

	for _, bad := range []string{`{"replicas":0}`, `{"replicas":11}`, `не json`} {
		if rec := do(t, h, "PUT", "/api/k8s/replicas", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", bad, rec.Code)
		}
	}

	rec := do(t, h, "PUT", "/api/k8s/replicas", `{"replicas":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("scale: %d %s", rec.Code, rec.Body)
	}
	want := `PATCH ` + nsApps + `/deployments/kube-demo/scale application/merge-patch+json {"spec":{"replicas":5}}`
	if got := f.last(); got != want {
		t.Errorf("request:\n got %s\nwant %s", got, want)
	}
	if cmd := decode[struct{ Command string }](t, rec).Command; cmd != "kubectl scale deployment/kube-demo --replicas=5" {
		t.Errorf("command %q", cmd)
	}
}

func TestRolloutRestart(t *testing.T) {
	f, h := newKubeTestServer(t)
	if rec := do(t, h, "POST", "/api/k8s/restart", ""); rec.Code != http.StatusOK {
		t.Fatalf("restart: %d %s", rec.Code, rec.Body)
	}
	if got := f.last(); !strings.Contains(got, "PATCH "+nsApps+"/deployments/kube-demo application/merge-patch+json") ||
		!strings.Contains(got, "kubectl.kubernetes.io/restartedAt") {
		t.Errorf("unexpected request: %s", got)
	}
}

func TestDeletePod(t *testing.T) {
	f, h := newKubeTestServer(t)

	if rec := do(t, h, "DELETE", "/api/k8s/pods/someone-else", ""); rec.Code != http.StatusForbidden {
		t.Errorf("foreign pod: got %d, want 403", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/k8s/pods/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing pod: got %d, want 404", rec.Code)
	}
	for _, name := range []string{"kube-demo-abc-1", "postgres-0"} {
		if rec := do(t, h, "DELETE", "/api/k8s/pods/"+name, ""); rec.Code != http.StatusAccepted {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body)
		}
		if got := f.last(); got != "DELETE "+nsPods+"/"+name {
			t.Errorf("unexpected request: %s", got)
		}
	}
}

func TestKubeDisabledOutsideCluster(t *testing.T) {
	_, h := newTestServer(t)
	for _, tc := range [][2]string{
		{"GET", "/api/k8s/state"}, {"PUT", "/api/k8s/replicas"}, {"POST", "/api/k8s/restart"}, {"DELETE", "/api/k8s/pods/x"},
	} {
		if rec := do(t, h, tc[0], tc[1], `{"replicas":2}`); rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: got %d, want 501", tc[0], tc[1], rec.Code)
		}
	}
}

func TestSnapshotSurvivesNewMessagesAndDetectsWriter(t *testing.T) {
	f, kube := newFakeKube(t)
	srv := newServer(Config{PodName: "kube-demo-abc-1"}, newMemoryStore(), func(int) {})
	srv.kube = kube
	h := srv.routes()

	do(t, h, "POST", "/api/messages", `{"text":"первое"}`)
	do(t, h, "POST", "/api/messages", `{"text":"второе"}`)

	rec := do(t, h, "POST", "/api/snapshots", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	snap := decode[Snapshot](t, rec)
	if snap.Messages != 2 || snap.MaxID != 2 || snap.Pod != "kube-demo-abc-1" || len(snap.Digest) != 64 {
		t.Fatalf("snapshot: %+v", snap)
	}

	// Новые сообщения после снимка не ломают проверку: сравниваются только id <= maxId.
	do(t, h, "POST", "/api/messages", `{"text":"после снимка"}`)

	check := decode[snapshotCheck](t, do(t, h, "GET", fmt.Sprintf("/api/snapshots/%d", snap.ID), ""))
	if !check.OK || check.CurrentMessages != 2 || check.WriterAlive == nil || !*check.WriterAlive {
		t.Fatalf("check: %+v", check)
	}

	// Под, сделавший снимок, удалён — проверка это показывает.
	f.mu.Lock()
	delete(f.pods, "kube-demo-abc-1")
	f.mu.Unlock()
	srv.cfg.PodName = "kube-demo-abc-9"
	check = decode[snapshotCheck](t, do(t, h, "GET", fmt.Sprintf("/api/snapshots/%d", snap.ID), ""))
	if !check.OK || check.WriterAlive == nil || *check.WriterAlive || check.CheckedBy != "kube-demo-abc-9" {
		t.Fatalf("check after writer is gone: %+v", check)
	}

	if rec := do(t, h, "GET", "/api/snapshots/42", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown snapshot: got %d, want 404", rec.Code)
	}
}

func TestSnapshotDetectsTampering(t *testing.T) {
	store := newMemoryStore()
	srv := newServer(Config{PodName: "p"}, store, func(int) {})
	h := srv.routes()

	do(t, h, "POST", "/api/messages", `{"text":"оригинал"}`)
	snap := decode[Snapshot](t, do(t, h, "POST", "/api/snapshots", ""))

	store.mu.Lock()
	store.messages[0].Text = "подменили"
	store.mu.Unlock()

	check := decode[snapshotCheck](t, do(t, h, "GET", fmt.Sprintf("/api/snapshots/%d", snap.ID), ""))
	if check.OK || check.WriterAlive != nil {
		t.Fatalf("tampering must be detected: %+v", check)
	}
}

func TestPodStatusJSONRoundTrip(t *testing.T) {
	// Защита от опечаток в json-тегах k8sPod: разбираем реальный кусок ответа API.
	var p k8sPod
	raw := `{"metadata":{"name":"x","deletionTimestamp":null},"status":{"containerStatuses":[{"ready":true,"restartCount":1}]}}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if v := toPodView(p); v.Restarts != 1 || v.Status != "Pending" {
		t.Fatalf("%+v", v)
	}
}
