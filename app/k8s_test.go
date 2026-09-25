package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeKube — маленький фейк Kubernetes API: ровно те эндпоинты, которые дёргает приложение.
type fakeKube struct {
	mu       sync.Mutex
	requests []string // "METHOD path content-type body"
	pods     map[string]string
	green    string // JSON деплоймента kube-demo-green; пусто — не развёрнут (404)
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
		"node-agent-x": `{"metadata":{"name":"node-agent-x","labels":{"app":"node-agent"},
			"ownerReferences":[{"kind":"DaemonSet","name":"node-agent"}]},"spec":{"containers":[{}]},"status":{}}`,
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
			fmt.Fprint(w, `{"metadata":{"name":"kube-demo","annotations":{"deployment.kubernetes.io/revision":"3"}},
				"spec":{"replicas":3,"template":{"metadata":{"annotations":{"kube-demo/release":"broken-1"}}}},
				"status":{"replicas":4,"readyReplicas":2,"updatedReplicas":1,"availableReplicas":2,
				  "conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded"}]}}`)
		case r.Method == "GET" && r.URL.Path == nsApps+"/deployments/kube-demo-green":
			if f.green == "" {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"deployments.apps \"kube-demo-green\" not found"}`)
				return
			}
			fmt.Fprint(w, f.green)
		case r.Method == "GET" && r.URL.Path == "/api/v1/namespaces/demo/services/kube-demo":
			fmt.Fprint(w, `{"spec":{"selector":{"app":"kube-demo"}}}`)
		case r.Method == "PATCH" && r.URL.Path == "/api/v1/namespaces/demo/services/kube-demo":
			fmt.Fprint(w, `{}`)
		case r.Method == "PUT" && r.URL.Path == nsApps+"/deployments/kube-demo":
			fmt.Fprint(w, `{}`)
		case r.Method == "GET" && r.URL.Path == nsApps+"/replicasets":
			if r.URL.Query().Get("labelSelector") != "app=kube-demo" {
				t.Errorf("unexpected labelSelector %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"items":[
				{"metadata":{"name":"kube-demo-old","creationTimestamp":"2026-01-01T00:00:00Z",
				  "annotations":{"deployment.kubernetes.io/revision":"1"}},"spec":{"replicas":0,
				  "template":{"metadata":{"labels":{"app":"kube-demo","pod-template-hash":"old"}}}},"status":{}},
				{"metadata":{"name":"kube-demo-broken","creationTimestamp":"2026-01-03T00:00:00Z",
				  "annotations":{"deployment.kubernetes.io/revision":"3"}},"spec":{"replicas":1,
				  "template":{"metadata":{"labels":{"app":"kube-demo","pod-template-hash":"broken"},
				    "annotations":{"kube-demo/release":"broken-1"}}}},"status":{"replicas":1}},
				{"metadata":{"name":"kube-demo-good","creationTimestamp":"2026-01-02T00:00:00Z",
				  "annotations":{"deployment.kubernetes.io/revision":"2"}},"spec":{"replicas":3,
				  "template":{"metadata":{"labels":{"app":"kube-demo","pod-template-hash":"good"}},
				    "spec":{"containers":[{"name":"app","image":"kube-demo:1.0.0"}]}}},"status":{"replicas":3,"readyReplicas":3}}]}`)
		case r.Method == "GET" && r.URL.Path == "/api/v1/namespaces/demo/events":
			fmt.Fprint(w, `{"items":[
				{"type":"Normal","reason":"Scheduled","message":"old","lastTimestamp":"2026-01-01T00:00:00Z",
				 "involvedObject":{"kind":"Pod","name":"a"}},
				{"type":"Warning","reason":"FailedCreate","message":"exceeded quota","count":4,
				 "lastTimestamp":"2026-01-02T00:00:00Z","involvedObject":{"kind":"ReplicaSet","name":"kube-demo-good"}}]}`)
		case r.Method == "GET" && r.URL.Path == "/apis/batch/v1/namespaces/demo/jobs":
			fmt.Fprint(w, `{"items":[
				{"metadata":{"name":"postgres-backup-1"},"status":{"succeeded":1,
				 "startTime":"2026-01-01T00:00:00Z","completionTime":"2026-01-01T00:00:07Z"}},
				{"metadata":{"name":"postgres-backup-manual-2","annotations":{"cronjob.kubernetes.io/instantiate":"manual"}},
				 "status":{"active":1,"startTime":"2026-01-02T00:00:00Z"}}]}`)
		case r.Method == "GET" && r.URL.Path == "/apis/batch/v1/namespaces/demo/cronjobs/postgres-backup":
			fmt.Fprint(w, `{"metadata":{"name":"postgres-backup","uid":"cj-uid"},
				"spec":{"schedule":"*/5 * * * *","jobTemplate":{"metadata":{"labels":{"app":"postgres-backup"}},
				  "spec":{"template":{"spec":{"containers":[{"name":"backup"}]}}}}}}`)
		case r.Method == "POST" && r.URL.Path == "/apis/batch/v1/namespaces/demo/jobs":
			fmt.Fprint(w, `{}`)
		case r.Method == "GET" && r.URL.Path == nsPods:
			var items []string
			for _, p := range f.pods {
				items = append(items, p)
			}
			fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
		case r.Method == "GET" && r.URL.Path == "/apis/autoscaling/v2/namespaces/demo/horizontalpodautoscalers":
			fmt.Fprint(w, `{"items":[
				{"metadata":{"name":"keda-hpa-kube-demo-rps"},
				 "spec":{"scaleTargetRef":{"name":"kube-demo"},"minReplicas":2,"maxReplicas":8,
				   "metrics":[{"type":"External","external":{"metric":{"name":"s0-prometheus"},
				     "target":{"type":"AverageValue","averageValue":"10"}}}]},
				 "status":{"currentReplicas":4,"desiredReplicas":5,"lastScaleTime":"2026-01-01T00:00:00Z",
				   "currentMetrics":[{"type":"External","external":{"metric":{"name":"s0-prometheus"},
				     "current":{"averageValue":"43250m"}}}]}},
				{"metadata":{"name":"cpu"},"spec":{"scaleTargetRef":{"name":"kube-demo"},"maxReplicas":8,
				   "metrics":[{"type":"Resource","resource":{"name":"cpu","target":{"type":"Utilization","averageUtilization":50}}},{}]},
				 "status":{"currentMetrics":[{"type":"Resource","resource":{"name":"cpu","current":{"averageUtilization":130}}}]}}]}`)
		case r.Method == "GET" && r.URL.Path == "/api/v1/nodes":
			fmt.Fprint(w, `{"items":[
				{"metadata":{"name":"kind-worker"},"spec":{"unschedulable":true},
				 "status":{"conditions":[{"type":"Ready","status":"Unknown","reason":"NodeStatusUnknown",
				   "lastHeartbeatTime":"2026-01-01T00:00:00Z"}],"nodeInfo":{"kubeletVersion":"v1.33.1"}}},
				{"metadata":{"name":"kind-control-plane","labels":{"node-role.kubernetes.io/control-plane":""}},
				 "spec":{"taints":[{"key":"node-role.kubernetes.io/control-plane","effect":"NoSchedule"}]},
				 "status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`)
		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/api/v1/nodes/"):
			fmt.Fprint(w, `{}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/eviction"):
			if strings.Contains(r.URL.Path, "postgres-0") {
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"message":"Cannot evict pod as it would violate the pod's disruption budget."}`)
				return
			}
			fmt.Fprint(w, `{}`)
		case r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/resize"):
			fmt.Fprint(w, `{}`)
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
	if len(got.ReplicaSets) != 3 || got.ReplicaSets[0].Name != "kube-demo-broken" || got.ReplicaSets[0].Release != "broken-1" {
		t.Errorf("replicasets should be sorted by revision, newest first: %+v", got.ReplicaSets)
	}
	if d := got.Deployment; d.Revision != "3" || d.Release != "broken-1" ||
		len(d.Conditions) != 1 || d.Conditions[0].Reason != "ProgressDeadlineExceeded" {
		t.Errorf("deployment rollout info: %+v", d)
	}
	if len(got.Events) != 2 || got.Events[0].Reason != "FailedCreate" || got.Events[0].Object != "replicaset/kube-demo-good" {
		t.Errorf("events should be newest first: %+v", got.Events)
	}
	if len(got.Jobs) != 2 || got.Jobs[0].Status != "Running" || !got.Jobs[0].Manual ||
		got.Jobs[1].Status != "Complete" || got.Jobs[1].Duration != "7s" {
		t.Errorf("jobs: %+v", got.Jobs)
	}
	if got.CronJob == nil || got.CronJob.Schedule != "*/5 * * * *" {
		t.Errorf("cronjob: %+v", got.CronJob)
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

func TestRolloutUndoUsesPreviousRevisionTemplate(t *testing.T) {
	f, h := newKubeTestServer(t)

	rec := do(t, h, "POST", "/api/k8s/undo", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body)
	}
	if rev := decode[struct{ ToRevision string }](t, rec).ToRevision; rev != "2" {
		t.Fatalf("should roll back to revision 2 (the one before current 3), got %q", rev)
	}
	req := f.last()
	if !strings.HasPrefix(req, "PUT "+nsApps+"/deployments/kube-demo application/json") {
		t.Fatalf("unexpected request: %s", req)
	}
	// Шаблон ревизии 2: без аннотации сломанного релиза и без pod-template-hash.
	for _, bad := range []string{"broken-1", "pod-template-hash"} {
		if strings.Contains(req, bad) {
			t.Errorf("template must not contain %q: %s", bad, req)
		}
	}
	if !strings.Contains(req, "kube-demo:1.0.0") {
		t.Errorf("template of revision 2 expected: %s", req)
	}
}

func TestBreakRelease(t *testing.T) {
	f, h := newKubeTestServer(t)
	if rec := do(t, h, "POST", "/api/k8s/break", ""); rec.Code != http.StatusOK {
		t.Fatalf("break: %d %s", rec.Code, rec.Body)
	}
	if got := f.last(); !strings.Contains(got, `"kube-demo/release":"broken-`) {
		t.Errorf("unexpected request: %s", got)
	}
}

func TestResizeCPU(t *testing.T) {
	f, h := newKubeTestServer(t)

	for _, bad := range []string{`{"limitMilli":50}`, `{"limitMilli":5000}`, `{}`} {
		if rec := do(t, h, "PUT", "/api/k8s/pods/kube-demo-abc-1/cpu", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", bad, rec.Code)
		}
	}
	if rec := do(t, h, "PUT", "/api/k8s/pods/postgres-0/cpu", `{"limitMilli":1000}`); rec.Code != http.StatusForbidden {
		t.Errorf("postgres resize: got %d, want 403", rec.Code)
	}

	rec := do(t, h, "PUT", "/api/k8s/pods/kube-demo-abc-1/cpu", `{"limitMilli":1000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resize: %d %s", rec.Code, rec.Body)
	}
	want := `PATCH ` + nsPods + `/kube-demo-abc-1/resize application/strategic-merge-patch+json ` +
		`{"spec":{"containers":[{"name":"app","resources":{"limits":{"cpu":"1000m"},"requests":{"cpu":"50m"}}}]}}`
	if got := f.last(); got != want {
		t.Errorf("request:\n got %s\nwant %s", got, want)
	}

	// Лимит ниже requests: requests опускается до лимита.
	do(t, h, "PUT", "/api/k8s/pods/kube-demo-abc-1/cpu", `{"limitMilli":100}`)
	if got := f.last(); !strings.Contains(got, `"limits":{"cpu":"100m"},"requests":{"cpu":"50m"}`) {
		t.Errorf("unexpected request: %s", got)
	}
}

func TestRunBackupCreatesJobFromCronJob(t *testing.T) {
	f, h := newKubeTestServer(t)
	rec := do(t, h, "POST", "/api/k8s/backup", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("backup: %d %s", rec.Code, rec.Body)
	}
	got := f.last()
	for _, want := range []string{
		"POST /apis/batch/v1/namespaces/demo/jobs",
		`"cronjob.kubernetes.io/instantiate":"manual"`,
		`"uid":"cj-uid"`,
		`"name":"backup"`,
		`"name":"postgres-backup-manual-`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("job request should contain %s: %s", want, got)
		}
	}
}

func TestPodViewSidecarAndLastState(t *testing.T) {
	var p k8sPod
	raw := `{"metadata":{"name":"x"},
		"spec":{"initContainers":[{"name":"wait"},{"name":"access-log","restartPolicy":"Always"}],
		        "containers":[{"name":"app","resources":{"requests":{"cpu":"50m"},"limits":{"cpu":"500m"}}}]},
		"status":{"phase":"Running",
		  "conditions":[{"type":"PodResizeInProgress","status":"True"}],
		  "initContainerStatuses":[
		    {"name":"wait","state":{"terminated":{"reason":"Completed"}}},
		    {"name":"access-log","ready":true,"state":{"running":{}}}],
		  "containerStatuses":[{"name":"app","ready":true,"restartCount":1,"state":{"running":{}},
		    "lastState":{"terminated":{"reason":"OOMKilled","exitCode":137,"finishedAt":"2026-01-01T00:00:00Z"}}}]}}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	v := toPodView(p)
	if v.Status != "Running" || v.Ready != "2/2" || v.Last != "OOMKilled (137)" || v.CPU != "50m/500m" || v.Resize != "InProgress" {
		t.Fatalf("%+v", v)
	}
}

func TestNodesInState(t *testing.T) {
	_, h := newKubeTestServer(t)
	st := decode[struct{ State clusterState }](t, do(t, h, "GET", "/api/k8s/state", "")).State

	if len(st.Nodes) != 2 || st.NodesError != "" {
		t.Fatalf("nodes: %+v (%s)", st.Nodes, st.NodesError)
	}
	cp, w := st.Nodes[0], st.Nodes[1] // control-plane первой
	if cp.Role != "control-plane" || cp.Status != "Ready" || len(cp.Taints) != 1 ||
		cp.Taints[0] != "node-role.kubernetes.io/control-plane:NoSchedule" {
		t.Errorf("control-plane: %+v", cp)
	}
	if w.Role != "worker" || w.Ready != "Unknown" || w.Status != "NotReady,SchedulingDisabled" ||
		w.Kubelet != "v1.33.1" || w.LastHeartbeat.IsZero() {
		t.Errorf("worker: %+v", w)
	}
}

func TestCordonAndDrain(t *testing.T) {
	f, h := newKubeTestServer(t)

	if rec := do(t, h, "POST", "/api/k8s/nodes/kind-worker/cordon", ""); rec.Code != http.StatusOK {
		t.Fatalf("cordon: %d %s", rec.Code, rec.Body)
	}
	if got := f.last(); got != `PATCH /api/v1/nodes/kind-worker application/merge-patch+json {"spec":{"unschedulable":true}}` {
		t.Errorf("cordon request: %s", got)
	}
	do(t, h, "POST", "/api/k8s/nodes/kind-worker/uncordon", "")
	if got := f.last(); !strings.HasSuffix(got, `{"spec":{"unschedulable":false}}`) {
		t.Errorf("uncordon request: %s", got)
	}
	if rec := do(t, h, "POST", "/api/k8s/nodes/kind-worker/reboot", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown action: got %d, want 404", rec.Code)
	}

	rec := do(t, h, "POST", "/api/k8s/nodes/kind-worker/drain", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("drain: %d %s", rec.Code, rec.Body)
	}
	res := decode[struct{ Result drainResult }](t, rec).Result
	// фейк отдаёт все поды: Terminating пропускаем, DaemonSet — skipped, postgres-0 упирается в PDB
	if !slices.Contains(res.Skipped, "node-agent-x") || slices.Contains(res.Evicted, "kube-demo-abc-2") ||
		!slices.Contains(res.Evicted, "kube-demo-abc-1") || len(res.Blocked) != 1 ||
		!strings.Contains(res.Blocked[0], "postgres-0 (PodDisruptionBudget)") {
		t.Fatalf("drain result: %+v", res)
	}
}

func TestHPAsInState(t *testing.T) {
	_, h := newKubeTestServer(t)
	st := decode[struct{ State clusterState }](t, do(t, h, "GET", "/api/k8s/state", "")).State
	if len(st.HPAs) != 2 {
		t.Fatalf("hpas: %+v", st.HPAs)
	}
	cpu, keda := st.HPAs[0], st.HPAs[1]
	if keda.Min != 2 || keda.Max != 8 || keda.Current != 4 || keda.Desired != 5 ||
		len(keda.Metrics) != 1 || keda.Metrics[0] != "s0-prometheus: 43.25 (avg) / 10 (avg)" {
		t.Errorf("keda hpa: %+v", keda)
	}
	// minReplicas по умолчанию 1; пустая метрика не должна ронять разбор
	if cpu.Min != 1 || len(cpu.Metrics) != 2 || cpu.Metrics[0] != "cpu: 130% / 50%" || cpu.Metrics[1] != "?: ? / ?" {
		t.Errorf("cpu hpa: %+v", cpu)
	}
}

func TestBlueGreen(t *testing.T) {
	f, h := newKubeTestServer(t)

	st := decode[struct{ State clusterState }](t, do(t, h, "GET", "/api/k8s/state", "")).State
	if bg := st.BlueGreen; bg == nil || bg.Active != "blue" || bg.Blue.Ready != 2 || bg.Green != nil {
		t.Fatalf("blue/green before green is deployed: %+v", bg)
	}
	if rec := do(t, h, "POST", "/api/k8s/bluegreen/green", ""); rec.Code != http.StatusNotFound {
		t.Errorf("switch to missing green: got %d, want 404", rec.Code)
	}

	f.mu.Lock()
	f.green = `{"metadata":{"name":"kube-demo-green"},"spec":{"replicas":2},"status":{"readyReplicas":0}}`
	f.mu.Unlock()
	rec := do(t, h, "POST", "/api/k8s/bluegreen/green", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "нет ни одного Ready-пода") {
		t.Errorf("switch to not ready green: got %d %s, want 409", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/k8s/bluegreen/purple", ""); rec.Code != http.StatusConflict {
		t.Errorf("unknown slot: got %d, want 409", rec.Code)
	}

	f.mu.Lock()
	f.green = `{"metadata":{"name":"kube-demo-green"},"spec":{"replicas":2},"status":{"readyReplicas":2}}`
	f.mu.Unlock()
	if rec := do(t, h, "POST", "/api/k8s/bluegreen/green", ""); rec.Code != http.StatusOK {
		t.Fatalf("switch to green: %d %s", rec.Code, rec.Body)
	}
	want := `PATCH /api/v1/namespaces/demo/services/kube-demo application/merge-patch+json {"spec":{"selector":{"app":"kube-demo-green"}}}`
	if got := f.last(); got != want {
		t.Errorf("request:\n got %s\nwant %s", got, want)
	}
	st = decode[struct{ State clusterState }](t, do(t, h, "GET", "/api/k8s/state", "")).State
	if g := st.BlueGreen.Green; g == nil || g.Ready != 2 || g.Desired != 2 {
		t.Errorf("green slot: %+v", g)
	}
}
