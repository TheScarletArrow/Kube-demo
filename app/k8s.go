package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Минимальный клиент Kubernetes API без client-go: приложению нужна дюжина
// запросов, и ради них не хочется тащить десятки мегабайт зависимостей.
//
// Авторизация — токен ServiceAccount, который kubelet монтирует в каждый под.
// Что именно разрешено этому ServiceAccount, описано в k8s/app/rbac.yaml.

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Аннотация шаблона пода, которой UI «выкатывает сломанный релиз».
// Через Downward API она попадает в env RELEASE_STATE (см. deployment.yaml).
const releaseAnnotation = "kube-demo/release"

var errNotInCluster = errors.New("приложение запущено не в Kubernetes: нет KUBERNETES_SERVICE_HOST")

type kubeClient struct {
	baseURL    string
	namespace  string
	deployment string
	http       *http.Client
	token      func() (string, error)
}

func newInClusterClient(namespace, deployment string) (*kubeClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errNotInCluster
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid CA certificate")
	}
	if namespace == "" || namespace == "-" {
		b, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			return nil, fmt.Errorf("read namespace: %w", err)
		}
		namespace = strings.TrimSpace(string(b))
	}

	return &kubeClient{
		baseURL:    "https://" + net.JoinHostPort(host, port),
		namespace:  namespace,
		deployment: deployment,
		http: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
		// Токен читаем на каждый запрос: kubelet периодически его ротирует.
		token: func() (string, error) {
			b, err := os.ReadFile(saDir + "/token")
			return strings.TrimSpace(string(b)), err
		},
	}, nil
}

type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("kubernetes API %d: %s", e.Code, e.Message) }

func isNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound
}

func (c *kubeClient) do(ctx context.Context, method, path, contentType string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	token, err := c.token()
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st)
		return &apiError{Code: resp.StatusCode, Message: st.Message}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *kubeClient) nsPath(group, resource string) string {
	if group == "" {
		return "/api/v1/namespaces/" + c.namespace + "/" + resource
	}
	return "/apis/" + group + "/namespaces/" + c.namespace + "/" + resource
}

// ---------- ответы Kubernetes API (только нужные поля) ----------

type k8sMeta struct {
	Name              string            `json:"name"`
	UID               string            `json:"uid"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"ownerReferences"`
}

type k8sResources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type k8sContainerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
	State        struct {
		Running *struct{} `json:"running"`
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
		Terminated *struct {
			Reason string `json:"reason"`
		} `json:"terminated"`
	} `json:"state"`
	LastState struct {
		Terminated *struct {
			Reason   string    `json:"reason"`
			ExitCode int32     `json:"exitCode"`
			Finished time.Time `json:"finishedAt"`
		} `json:"terminated"`
	} `json:"lastState"`
}

type k8sContainer struct {
	Name          string       `json:"name"`
	RestartPolicy string       `json:"restartPolicy"` // "Always" у init-контейнера = нативный sidecar
	Resources     k8sResources `json:"resources"`
}

type k8sPod struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		NodeName       string         `json:"nodeName"`
		InitContainers []k8sContainer `json:"initContainers"`
		Containers     []k8sContainer `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase                 string               `json:"phase"`
		PodIP                 string               `json:"podIP"`
		InitContainerStatuses []k8sContainerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []k8sContainerStatus `json:"containerStatuses"`
		Conditions            []k8sCondition       `json:"conditions"`
	} `json:"status"`
}

type k8sCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type k8sDeployment struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		Replicas int32 `json:"replicas"`
		Template struct {
			Metadata k8sMeta `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas          int32          `json:"replicas"`
		ReadyReplicas     int32          `json:"readyReplicas"`
		UpdatedReplicas   int32          `json:"updatedReplicas"`
		AvailableReplicas int32          `json:"availableReplicas"`
		Conditions        []k8sCondition `json:"conditions"`
	} `json:"status"`
}

type k8sReplicaSet struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		Replicas int32           `json:"replicas"`
		Template json.RawMessage `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas      int32 `json:"replicas"`
		ReadyReplicas int32 `json:"readyReplicas"`
	} `json:"status"`
}

type k8sEvent struct {
	Type           string    `json:"type"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	Count          int32     `json:"count"`
	LastTimestamp  time.Time `json:"lastTimestamp"`
	EventTime      time.Time `json:"eventTime"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
	Metadata k8sMeta `json:"metadata"`
}

type k8sJob struct {
	Metadata k8sMeta `json:"metadata"`
	Status   struct {
		Active         int32      `json:"active"`
		Succeeded      int32      `json:"succeeded"`
		Failed         int32      `json:"failed"`
		StartTime      *time.Time `json:"startTime"`
		CompletionTime *time.Time `json:"completionTime"`
	} `json:"status"`
}

type k8sCronJob struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		Schedule    string `json:"schedule"`
		Suspend     bool   `json:"suspend"`
		JobTemplate struct {
			Metadata k8sMeta        `json:"metadata"`
			Spec     map[string]any `json:"spec"`
		} `json:"jobTemplate"`
	} `json:"spec"`
	Status struct {
		LastScheduleTime   *time.Time `json:"lastScheduleTime"`
		LastSuccessfulTime *time.Time `json:"lastSuccessfulTime"`
	} `json:"status"`
}

// ---------- то, что отдаём в UI (как колонки kubectl get) ----------

type podView struct {
	Name     string `json:"name"`
	App      string `json:"app"`
	Owner    string `json:"owner"`
	Ready    string `json:"ready"` // "2/2"
	Status   string `json:"status"`
	Restarts int32  `json:"restarts"`
	Last     string `json:"last"` // причина последнего падения: "OOMKilled (137)"
	Node     string `json:"node"`
	IP       string `json:"ip"`
	CPU      string `json:"cpu"` // requests/limits контейнера app: "50m/500m"
	Resize   string `json:"resize,omitempty"`
	// PodReady — condition Ready пода. Когда нода умирает, kubelet уже ничего
	// не обновит (READY у kubectl «застывает»), а node controller сбросит именно его.
	PodReady  bool      `json:"podReady"`
	CreatedAt time.Time `json:"createdAt"`
}

type deploymentView struct {
	Name       string         `json:"name"`
	Desired    int32          `json:"desired"`
	Current    int32          `json:"current"`
	Ready      int32          `json:"ready"`
	Updated    int32          `json:"updated"`
	Available  int32          `json:"available"`
	Revision   string         `json:"revision"`
	Release    string         `json:"release"` // значение аннотации kube-demo/release в шаблоне
	Conditions []k8sCondition `json:"conditions"`
}

type replicaSetView struct {
	Name      string    `json:"name"`
	Revision  string    `json:"revision"`
	Release   string    `json:"release"`
	Desired   int32     `json:"desired"`
	Current   int32     `json:"current"`
	Ready     int32     `json:"ready"`
	CreatedAt time.Time `json:"createdAt"`
}

type eventView struct {
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Object  string    `json:"object"`
	Message string    `json:"message"`
	Count   int32     `json:"count"`
	Time    time.Time `json:"time"`
}

type jobView struct {
	Name      string     `json:"name"`
	Status    string     `json:"status"` // Running | Complete | Failed
	Manual    bool       `json:"manual"`
	StartTime *time.Time `json:"startTime"`
	Duration  string     `json:"duration"`
}

type cronJobView struct {
	Name               string     `json:"name"`
	Schedule           string     `json:"schedule"`
	LastScheduleTime   *time.Time `json:"lastScheduleTime"`
	LastSuccessfulTime *time.Time `json:"lastSuccessfulTime"`
}

type clusterState struct {
	Namespace   string           `json:"namespace"`
	Deployment  deploymentView   `json:"deployment"`
	ReplicaSets []replicaSetView `json:"replicaSets"`
	Pods        []podView        `json:"pods"`
	Events      []eventView      `json:"events"`
	Jobs        []jobView        `json:"jobs"`
	CronJob     *cronJobView     `json:"cronJob"`
	Nodes       []nodeView       `json:"nodes"`
	HPAs        []hpaView        `json:"hpas"`
	NodesError  string           `json:"nodesError,omitempty"`
}

func isSidecar(c k8sContainer) bool { return c.RestartPolicy == "Always" }

// podStatus повторяет логику колонки STATUS в `kubectl get pods`.
func podStatus(p k8sPod) string {
	if p.Metadata.DeletionTimestamp != nil {
		return "Terminating"
	}
	sidecars := map[string]bool{}
	for _, c := range p.Spec.InitContainers {
		sidecars[c.Name] = isSidecar(c)
	}
	for i, cs := range p.Status.InitContainerStatuses {
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.Reason == "Completed":
			continue
		case sidecars[cs.Name] && cs.State.Running != nil:
			continue // запущенный sidecar — это норма, а не «инициализация»
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing":
			return "Init:" + cs.State.Waiting.Reason
		default:
			return fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}
	if p.Status.Phase == "" {
		return "Pending"
	}
	return p.Status.Phase
}

func toPodView(p k8sPod) podView {
	var ready, total int
	var restarts int32
	var last string
	var lastAt time.Time

	count := func(cs k8sContainerStatus) {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
		if t := cs.LastState.Terminated; t != nil && t.Finished.After(lastAt) {
			lastAt = t.Finished
			last = fmt.Sprintf("%s (%d)", t.Reason, t.ExitCode)
		}
	}
	// READY у kubectl считает обычные контейнеры и нативные sidecar'ы.
	sidecars := map[string]bool{}
	for _, c := range p.Spec.InitContainers {
		if isSidecar(c) {
			sidecars[c.Name] = true
			total++
		}
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if sidecars[cs.Name] {
			count(cs)
		}
	}
	total += len(p.Spec.Containers)
	for _, cs := range p.Status.ContainerStatuses {
		count(cs)
	}

	owner := ""
	if len(p.Metadata.OwnerReferences) > 0 {
		owner = p.Metadata.OwnerReferences[0].Kind + "/" + p.Metadata.OwnerReferences[0].Name
	}
	cpu := ""
	if len(p.Spec.Containers) > 0 {
		r := p.Spec.Containers[0].Resources
		if r.Requests["cpu"] != "" || r.Limits["cpu"] != "" {
			cpu = orDash(r.Requests["cpu"]) + "/" + orDash(r.Limits["cpu"])
		}
	}
	resize := ""
	podReady := false
	for _, c := range p.Status.Conditions {
		if (c.Type == "PodResizePending" || c.Type == "PodResizeInProgress") && c.Status == "True" {
			resize = strings.TrimPrefix(c.Type, "PodResize")
		}
		if c.Type == "Ready" && c.Status == "True" {
			podReady = true
		}
	}
	return podView{
		Name:      p.Metadata.Name,
		App:       p.Metadata.Labels["app"],
		Owner:     owner,
		Ready:     fmt.Sprintf("%d/%d", ready, total),
		Status:    podStatus(p),
		Restarts:  restarts,
		Last:      last,
		Node:      p.Spec.NodeName,
		IP:        p.Status.PodIP,
		CPU:       cpu,
		Resize:    resize,
		PodReady:  podReady,
		CreatedAt: p.Metadata.CreationTimestamp,
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// State ≈ kubectl get deploy,rs,pods,events,jobs,cronjobs
func (c *kubeClient) State(ctx context.Context) (clusterState, error) {
	st := clusterState{
		Namespace:   c.namespace,
		ReplicaSets: []replicaSetView{},
		Pods:        []podView{},
		Events:      []eventView{},
		Jobs:        []jobView{},
		Nodes:       []nodeView{},
		HPAs:        []hpaView{},
	}

	var d k8sDeployment
	if err := c.do(ctx, "GET", c.nsPath("apps/v1", "deployments/"+c.deployment), "", nil, &d); err != nil {
		return st, err
	}
	st.Deployment = deploymentView{
		Name:       d.Metadata.Name,
		Desired:    d.Spec.Replicas,
		Current:    d.Status.Replicas,
		Ready:      d.Status.ReadyReplicas,
		Updated:    d.Status.UpdatedReplicas,
		Available:  d.Status.AvailableReplicas,
		Revision:   d.Metadata.Annotations["deployment.kubernetes.io/revision"],
		Release:    d.Spec.Template.Metadata.Annotations[releaseAnnotation],
		Conditions: d.Status.Conditions,
	}

	rsList, err := c.replicaSets(ctx)
	if err != nil {
		return st, err
	}
	for _, rs := range rsList {
		var tpl struct {
			Metadata k8sMeta `json:"metadata"`
		}
		_ = json.Unmarshal(rs.Spec.Template, &tpl)
		st.ReplicaSets = append(st.ReplicaSets, replicaSetView{
			Name:      rs.Metadata.Name,
			Revision:  rs.Metadata.Annotations["deployment.kubernetes.io/revision"],
			Release:   tpl.Metadata.Annotations[releaseAnnotation],
			Desired:   rs.Spec.Replicas,
			Current:   rs.Status.Replicas,
			Ready:     rs.Status.ReadyReplicas,
			CreatedAt: rs.Metadata.CreationTimestamp,
		})
	}

	var podList struct {
		Items []k8sPod `json:"items"`
	}
	if err := c.do(ctx, "GET", c.nsPath("", "pods"), "", nil, &podList); err != nil {
		return st, err
	}
	for _, p := range podList.Items {
		st.Pods = append(st.Pods, toPodView(p))
	}
	slices.SortFunc(st.Pods, func(a, b podView) int { return strings.Compare(a.Name, b.Name) })

	// События, джобы и CronJob — «best effort»: если RBAC их не даёт, остальное всё равно показываем.
	if ev, err := c.Events(ctx, 12); err == nil {
		st.Events = ev
	}
	if jobs, err := c.Jobs(ctx); err == nil {
		st.Jobs = jobs
	}
	if hpas, err := c.HPAs(ctx); err == nil {
		st.HPAs = hpas
	}
	if nodes, err := c.Nodes(ctx); err == nil {
		st.Nodes = nodes
	} else {
		st.NodesError = err.Error()
	}
	if cj, err := c.CronJob(ctx, backupCronJob); err == nil {
		st.CronJob = &cronJobView{
			Name:               cj.Metadata.Name,
			Schedule:           cj.Spec.Schedule,
			LastScheduleTime:   cj.Status.LastScheduleTime,
			LastSuccessfulTime: cj.Status.LastSuccessfulTime,
		}
	}
	return st, nil
}

// replicaSets возвращает ReplicaSet'ы деплоймента, новые — первыми.
func (c *kubeClient) replicaSets(ctx context.Context) ([]k8sReplicaSet, error) {
	var list struct {
		Items []k8sReplicaSet `json:"items"`
	}
	q := "?labelSelector=" + url.QueryEscape("app="+c.deployment)
	if err := c.do(ctx, "GET", c.nsPath("apps/v1", "replicasets")+q, "", nil, &list); err != nil {
		return nil, err
	}
	rev := func(rs k8sReplicaSet) int {
		n, _ := strconv.Atoi(rs.Metadata.Annotations["deployment.kubernetes.io/revision"])
		return n
	}
	slices.SortFunc(list.Items, func(a, b k8sReplicaSet) int { return rev(b) - rev(a) })
	return list.Items, nil
}

// Scale ≈ kubectl scale deployment/<name> --replicas=N
func (c *kubeClient) Scale(ctx context.Context, replicas int32) error {
	patch := map[string]any{"spec": map[string]any{"replicas": replicas}}
	return c.do(ctx, "PATCH", c.nsPath("apps/v1", "deployments/"+c.deployment+"/scale"),
		"application/merge-patch+json", patch, nil)
}

func (c *kubeClient) patchTemplateAnnotations(ctx context.Context, ann map[string]any) error {
	patch := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": ann,
	}}}}
	return c.do(ctx, "PATCH", c.nsPath("apps/v1", "deployments/"+c.deployment),
		"application/merge-patch+json", patch, nil)
}

// RolloutRestart ≈ kubectl rollout restart deployment/<name>:
// kubectl делает ровно это — меняет аннотацию в шаблоне пода.
func (c *kubeClient) RolloutRestart(ctx context.Context) error {
	return c.patchTemplateAnnotations(ctx, map[string]any{
		"kubectl.kubernetes.io/restartedAt": time.Now().Format(time.RFC3339),
	})
}

// BreakRelease выкатывает «сломанный релиз»: новая аннотация шаблона ->
// новый ReplicaSet, а его поды через Downward API узнают, что они сломаны,
// и никогда не проходят readiness.
func (c *kubeClient) BreakRelease(ctx context.Context) error {
	return c.patchTemplateAnnotations(ctx, map[string]any{releaseAnnotation: "broken-" + time.Now().Format("150405")})
}

// RolloutUndo ≈ kubectl rollout undo deployment/<name>.
// kubectl делает то же самое на клиенте: берёт шаблон пода из ReplicaSet
// предыдущей ревизии и записывает его в Deployment.
func (c *kubeClient) RolloutUndo(ctx context.Context) (string, error) {
	var d map[string]any
	if err := c.do(ctx, "GET", c.nsPath("apps/v1", "deployments/"+c.deployment), "", nil, &d); err != nil {
		return "", err
	}
	meta, _ := d["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	current, _ := strconv.Atoi(fmt.Sprint(ann["deployment.kubernetes.io/revision"]))

	rsList, err := c.replicaSets(ctx)
	if err != nil {
		return "", err
	}
	for _, rs := range rsList { // от новых к старым
		rev, _ := strconv.Atoi(rs.Metadata.Annotations["deployment.kubernetes.io/revision"])
		if rev == 0 || rev >= current || len(rs.Spec.Template) == 0 {
			continue
		}
		var tpl map[string]any
		if err := json.Unmarshal(rs.Spec.Template, &tpl); err != nil {
			return "", err
		}
		if m, ok := tpl["metadata"].(map[string]any); ok {
			if labels, ok := m["labels"].(map[string]any); ok {
				delete(labels, "pod-template-hash") // его проставляет контроллер RS
			}
		}
		d["spec"].(map[string]any)["template"] = tpl
		if err := c.do(ctx, "PUT", c.nsPath("apps/v1", "deployments/"+c.deployment), "application/json", d, nil); err != nil {
			return "", err
		}
		return strconv.Itoa(rev), nil
	}
	return "", errors.New("нет предыдущей ревизии для отката")
}

// GetPod ≈ kubectl get pod <name>
func (c *kubeClient) GetPod(ctx context.Context, name string) (k8sPod, error) {
	var p k8sPod
	err := c.do(ctx, "GET", c.nsPath("", "pods/"+url.PathEscape(name)), "", nil, &p)
	return p, err
}

// DeletePod ≈ kubectl delete pod <name> --wait=false
func (c *kubeClient) DeletePod(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", c.nsPath("", "pods/"+url.PathEscape(name)), "", nil, nil)
}

// ResizePodCPU ≈ kubectl patch pod <name> --subresource resize ...
// In-place resize: ресурсы меняются у живого контейнера, без рестарта.
func (c *kubeClient) ResizePodCPU(ctx context.Context, name, container, request, limit string) error {
	patch := map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
		"name":      container,
		"resources": map[string]any{"requests": map[string]string{"cpu": request}, "limits": map[string]string{"cpu": limit}},
	}}}}
	return c.do(ctx, "PATCH", c.nsPath("", "pods/"+url.PathEscape(name)+"/resize"),
		"application/strategic-merge-patch+json", patch, nil)
}

// Events ≈ kubectl get events --sort-by=.lastTimestamp | tail
func (c *kubeClient) Events(ctx context.Context, limit int) ([]eventView, error) {
	var list struct {
		Items []k8sEvent `json:"items"`
	}
	if err := c.do(ctx, "GET", c.nsPath("", "events"), "", nil, &list); err != nil {
		return nil, err
	}
	out := make([]eventView, 0, len(list.Items))
	for _, e := range list.Items {
		t := e.LastTimestamp
		if t.IsZero() {
			t = e.EventTime
		}
		if t.IsZero() {
			t = e.Metadata.CreationTimestamp
		}
		out = append(out, eventView{
			Type:    e.Type,
			Reason:  e.Reason,
			Object:  strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name,
			Message: e.Message,
			Count:   max(e.Count, 1),
			Time:    t,
		})
	}
	slices.SortFunc(out, func(a, b eventView) int { return b.Time.Compare(a.Time) })
	return out[:min(limit, len(out))], nil
}

// Jobs ≈ kubectl get jobs
func (c *kubeClient) Jobs(ctx context.Context) ([]jobView, error) {
	var list struct {
		Items []k8sJob `json:"items"`
	}
	if err := c.do(ctx, "GET", c.nsPath("batch/v1", "jobs"), "", nil, &list); err != nil {
		return nil, err
	}
	out := []jobView{}
	for _, j := range list.Items {
		v := jobView{
			Name:      j.Metadata.Name,
			Status:    "Running",
			Manual:    j.Metadata.Annotations["cronjob.kubernetes.io/instantiate"] == "manual",
			StartTime: j.Status.StartTime,
		}
		switch {
		case j.Status.Succeeded > 0:
			v.Status = "Complete"
		case j.Status.Failed > 0 && j.Status.Active == 0:
			v.Status = "Failed"
		}
		if j.Status.StartTime != nil {
			end := time.Now()
			if j.Status.CompletionTime != nil {
				end = *j.Status.CompletionTime
			}
			v.Duration = end.Sub(*j.Status.StartTime).Round(time.Second).String()
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b jobView) int {
		if a.StartTime == nil || b.StartTime == nil {
			return strings.Compare(b.Name, a.Name)
		}
		return b.StartTime.Compare(*a.StartTime)
	})
	return out, nil
}

func (c *kubeClient) CronJob(ctx context.Context, name string) (k8sCronJob, error) {
	var cj k8sCronJob
	err := c.do(ctx, "GET", c.nsPath("batch/v1", "cronjobs/"+name), "", nil, &cj)
	return cj, err
}

// RunCronJobNow ≈ kubectl create job <name>-manual-xxx --from=cronjob/<name>
func (c *kubeClient) RunCronJobNow(ctx context.Context, name string) (string, error) {
	cj, err := c.CronJob(ctx, name)
	if err != nil {
		return "", err
	}
	jobName := fmt.Sprintf("%s-manual-%d", name, time.Now().Unix())
	labels := cj.Spec.JobTemplate.Metadata.Labels
	job := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        jobName,
			"labels":      labels,
			"annotations": map[string]string{"cronjob.kubernetes.io/instantiate": "manual"},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "batch/v1", "kind": "CronJob", "name": cj.Metadata.Name, "uid": cj.Metadata.UID,
			}},
		},
		"spec": cj.Spec.JobTemplate.Spec,
	}
	return jobName, c.do(ctx, "POST", c.nsPath("batch/v1", "jobs"), "application/json", job, nil)
}
