package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Ноды — ресурс уровня кластера (не namespace'а), поэтому на них нужен
// ClusterRole, а не Role (см. k8s/app/rbac.yaml).

type k8sNode struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
		Taints        []struct {
			Key    string `json:"key"`
			Value  string `json:"value"`
			Effect string `json:"effect"`
		} `json:"taints"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type              string    `json:"type"`
			Status            string    `json:"status"`
			Reason            string    `json:"reason"`
			LastHeartbeatTime time.Time `json:"lastHeartbeatTime"`
		} `json:"conditions"`
		NodeInfo struct {
			KubeletVersion string `json:"kubeletVersion"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type nodeView struct {
	Name          string    `json:"name"`
	Role          string    `json:"role"`   // control-plane | worker
	Ready         string    `json:"ready"`  // True | False | Unknown — как condition Ready
	Status        string    `json:"status"` // как колонка STATUS у kubectl get nodes
	Unschedulable bool      `json:"unschedulable"`
	Taints        []string  `json:"taints"`
	Kubelet       string    `json:"kubelet"`
	LastHeartbeat time.Time `json:"lastHeartbeat"`
	CreatedAt     time.Time `json:"createdAt"`
}

func toNodeView(n k8sNode) nodeView {
	v := nodeView{
		Name:          n.Metadata.Name,
		Role:          "worker",
		Ready:         "Unknown",
		Unschedulable: n.Spec.Unschedulable,
		Taints:        []string{},
		Kubelet:       n.Status.NodeInfo.KubeletVersion,
		CreatedAt:     n.Metadata.CreationTimestamp,
	}
	if _, ok := n.Metadata.Labels["node-role.kubernetes.io/control-plane"]; ok {
		v.Role = "control-plane"
	}
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			v.Ready = c.Status
			v.LastHeartbeat = c.LastHeartbeatTime
		}
	}
	for _, t := range n.Spec.Taints {
		s := t.Key
		if t.Value != "" {
			s += "=" + t.Value
		}
		v.Taints = append(v.Taints, s+":"+t.Effect)
	}

	v.Status = "NotReady"
	if v.Ready == "True" {
		v.Status = "Ready"
	}
	if v.Unschedulable {
		v.Status += ",SchedulingDisabled"
	}
	return v
}

// Nodes ≈ kubectl get nodes
func (c *kubeClient) Nodes(ctx context.Context) ([]nodeView, error) {
	var list struct {
		Items []k8sNode `json:"items"`
	}
	if err := c.do(ctx, "GET", "/api/v1/nodes", "", nil, &list); err != nil {
		return nil, err
	}
	out := make([]nodeView, 0, len(list.Items))
	for _, n := range list.Items {
		out = append(out, toNodeView(n))
	}
	// control-plane первой, дальше воркеры по имени
	slices.SortFunc(out, func(a, b nodeView) int {
		if a.Role != b.Role {
			return strings.Compare(a.Role, b.Role)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// Cordon ≈ kubectl cordon|uncordon <node>: запретить/разрешить планировать новые поды.
func (c *kubeClient) Cordon(ctx context.Context, node string, cordon bool) error {
	patch := map[string]any{"spec": map[string]any{"unschedulable": cordon}}
	return c.do(ctx, "PATCH", "/api/v1/nodes/"+url.PathEscape(node), "application/merge-patch+json", patch, nil)
}

// Evict — «вежливое» удаление пода через Eviction API. В отличие от DELETE
// оно учитывает PodDisruptionBudget: если бюджет исчерпан, API ответит 429.
func (c *kubeClient) Evict(ctx context.Context, pod string) error {
	body := map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "Eviction",
		"metadata":   map[string]string{"name": pod, "namespace": c.namespace},
	}
	return c.do(ctx, "POST", c.nsPath("", "pods/"+url.PathEscape(pod)+"/eviction"), "application/json", body, nil)
}

type drainResult struct {
	Evicted []string `json:"evicted"`
	Blocked []string `json:"blocked"` // не выселены: PDB или ошибка
	Skipped []string `json:"skipped"` // поды DaemonSet'ов — как --ignore-daemonsets
}

// Drain ≈ kubectl drain <node> --ignore-daemonsets, но только для подов
// нашего namespace: cordon + Eviction каждого пода на этой ноде.
func (c *kubeClient) Drain(ctx context.Context, node string) (drainResult, error) {
	res := drainResult{Evicted: []string{}, Blocked: []string{}, Skipped: []string{}}
	if err := c.Cordon(ctx, node, true); err != nil {
		return res, fmt.Errorf("cordon: %w", err)
	}

	var list struct {
		Items []k8sPod `json:"items"`
	}
	q := "?fieldSelector=" + url.QueryEscape("spec.nodeName="+node)
	if err := c.do(ctx, "GET", c.nsPath("", "pods")+q, "", nil, &list); err != nil {
		return res, err
	}
	for _, p := range list.Items {
		name := p.Metadata.Name
		switch {
		case p.Metadata.DeletionTimestamp != nil:
			continue
		case len(p.Metadata.OwnerReferences) > 0 && p.Metadata.OwnerReferences[0].Kind == "DaemonSet":
			res.Skipped = append(res.Skipped, name)
			continue
		}
		if err := c.Evict(ctx, name); err != nil {
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Code == 429 {
				res.Blocked = append(res.Blocked, name+" (PodDisruptionBudget)")
			} else {
				res.Blocked = append(res.Blocked, name+": "+err.Error())
			}
			continue
		}
		res.Evicted = append(res.Evicted, name)
	}
	return res, nil
}
