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
	"strings"
	"time"
)

// Минимальный клиент Kubernetes API без client-go: приложению нужно
// всего пять запросов, и ради них не хочется тащить десятки мегабайт зависимостей.
//
// Авторизация — токен ServiceAccount, который kubelet монтирует в каждый под.
// Что именно разрешено этому ServiceAccount, описано в k8s/app/rbac.yaml.

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

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

// ---------- ответы Kubernetes API (только нужные поля) ----------

type k8sMeta struct {
	Name              string            `json:"name"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"ownerReferences"`
}

type k8sContainerStatus struct {
	Ready        bool  `json:"ready"`
	RestartCount int32 `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
		Terminated *struct {
			Reason string `json:"reason"`
		} `json:"terminated"`
	} `json:"state"`
}

type k8sPod struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		NodeName       string     `json:"nodeName"`
		InitContainers []struct{} `json:"initContainers"`
		Containers     []struct{} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase                 string               `json:"phase"`
		PodIP                 string               `json:"podIP"`
		InitContainerStatuses []k8sContainerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []k8sContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type k8sDeployment struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		Replicas int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		Replicas          int32 `json:"replicas"`
		ReadyReplicas     int32 `json:"readyReplicas"`
		UpdatedReplicas   int32 `json:"updatedReplicas"`
		AvailableReplicas int32 `json:"availableReplicas"`
	} `json:"status"`
}

type k8sReplicaSet struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		Replicas int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		Replicas      int32 `json:"replicas"`
		ReadyReplicas int32 `json:"readyReplicas"`
	} `json:"status"`
}

// ---------- то, что отдаём в UI (как колонки kubectl get) ----------

type podView struct {
	Name      string    `json:"name"`
	App       string    `json:"app"`
	Owner     string    `json:"owner"`
	Ready     string    `json:"ready"` // "1/1"
	Status    string    `json:"status"`
	Restarts  int32     `json:"restarts"`
	Node      string    `json:"node"`
	IP        string    `json:"ip"`
	CreatedAt time.Time `json:"createdAt"`
}

type deploymentView struct {
	Name      string `json:"name"`
	Desired   int32  `json:"desired"`
	Current   int32  `json:"current"`
	Ready     int32  `json:"ready"`
	Updated   int32  `json:"updated"`
	Available int32  `json:"available"`
}

type replicaSetView struct {
	Name      string    `json:"name"`
	Revision  string    `json:"revision"`
	Desired   int32     `json:"desired"`
	Current   int32     `json:"current"`
	Ready     int32     `json:"ready"`
	CreatedAt time.Time `json:"createdAt"`
}

type clusterState struct {
	Namespace   string           `json:"namespace"`
	Deployment  deploymentView   `json:"deployment"`
	ReplicaSets []replicaSetView `json:"replicaSets"`
	Pods        []podView        `json:"pods"`
}

// podStatus повторяет логику колонки STATUS в `kubectl get pods`.
func podStatus(p k8sPod) string {
	if p.Metadata.DeletionTimestamp != nil {
		return "Terminating"
	}
	for i, cs := range p.Status.InitContainerStatuses {
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.Reason == "Completed":
			continue
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
	var ready int
	var restarts int32
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
	}
	owner := ""
	if len(p.Metadata.OwnerReferences) > 0 {
		owner = p.Metadata.OwnerReferences[0].Kind + "/" + p.Metadata.OwnerReferences[0].Name
	}
	return podView{
		Name:      p.Metadata.Name,
		App:       p.Metadata.Labels["app"],
		Owner:     owner,
		Ready:     fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers)),
		Status:    podStatus(p),
		Restarts:  restarts,
		Node:      p.Spec.NodeName,
		IP:        p.Status.PodIP,
		CreatedAt: p.Metadata.CreationTimestamp,
	}
}

func (c *kubeClient) nsPath(group, resource string) string {
	if group == "" {
		return "/api/v1/namespaces/" + c.namespace + "/" + resource
	}
	return "/apis/" + group + "/namespaces/" + c.namespace + "/" + resource
}

// State ≈ kubectl get deploy,rs,pods
func (c *kubeClient) State(ctx context.Context) (clusterState, error) {
	st := clusterState{Namespace: c.namespace, ReplicaSets: []replicaSetView{}, Pods: []podView{}}

	var d k8sDeployment
	if err := c.do(ctx, "GET", c.nsPath("apps/v1", "deployments/"+c.deployment), "", nil, &d); err != nil {
		return st, err
	}
	st.Deployment = deploymentView{
		Name:      d.Metadata.Name,
		Desired:   d.Spec.Replicas,
		Current:   d.Status.Replicas,
		Ready:     d.Status.ReadyReplicas,
		Updated:   d.Status.UpdatedReplicas,
		Available: d.Status.AvailableReplicas,
	}

	var rsList struct {
		Items []k8sReplicaSet `json:"items"`
	}
	q := "?labelSelector=" + url.QueryEscape("app="+c.deployment)
	if err := c.do(ctx, "GET", c.nsPath("apps/v1", "replicasets")+q, "", nil, &rsList); err != nil {
		return st, err
	}
	for _, rs := range rsList.Items {
		st.ReplicaSets = append(st.ReplicaSets, replicaSetView{
			Name:      rs.Metadata.Name,
			Revision:  rs.Metadata.Annotations["deployment.kubernetes.io/revision"],
			Desired:   rs.Spec.Replicas,
			Current:   rs.Status.Replicas,
			Ready:     rs.Status.ReadyReplicas,
			CreatedAt: rs.Metadata.CreationTimestamp,
		})
	}
	slices.SortFunc(st.ReplicaSets, func(a, b replicaSetView) int { return b.CreatedAt.Compare(a.CreatedAt) })

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
	return st, nil
}

// Scale ≈ kubectl scale deployment/<name> --replicas=N
func (c *kubeClient) Scale(ctx context.Context, replicas int32) error {
	patch := map[string]any{"spec": map[string]any{"replicas": replicas}}
	return c.do(ctx, "PATCH", c.nsPath("apps/v1", "deployments/"+c.deployment+"/scale"),
		"application/merge-patch+json", patch, nil)
}

// RolloutRestart ≈ kubectl rollout restart deployment/<name>:
// kubectl делает ровно это — меняет аннотацию в шаблоне пода.
func (c *kubeClient) RolloutRestart(ctx context.Context) error {
	patch := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{"kubectl.kubernetes.io/restartedAt": time.Now().Format(time.RFC3339)},
	}}}}
	return c.do(ctx, "PATCH", c.nsPath("apps/v1", "deployments/"+c.deployment),
		"application/merge-patch+json", patch, nil)
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
