package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// HorizontalPodAutoscaler'ы namespace'а: и «ручной» HPA по CPU
// (k8s/extras/hpa.yaml), и тот, что создаёт KEDA для автоскейлинга по RPS.

type k8sHPA struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		ScaleTargetRef struct {
			Name string `json:"name"`
		} `json:"scaleTargetRef"`
		MinReplicas *int32           `json:"minReplicas"`
		MaxReplicas int32            `json:"maxReplicas"`
		Metrics     []map[string]any `json:"metrics"`
	} `json:"spec"`
	Status struct {
		CurrentReplicas int32            `json:"currentReplicas"`
		DesiredReplicas int32            `json:"desiredReplicas"`
		LastScaleTime   *time.Time       `json:"lastScaleTime"`
		CurrentMetrics  []map[string]any `json:"currentMetrics"`
	} `json:"status"`
}

type hpaView struct {
	Name          string     `json:"name"`
	Target        string     `json:"target"`
	Min           int32      `json:"min"`
	Max           int32      `json:"max"`
	Current       int32      `json:"current"`
	Desired       int32      `json:"desired"`
	Metrics       []string   `json:"metrics"` // "s0-prometheus: 43.2 / 10 (avg)"
	LastScaleTime *time.Time `json:"lastScaleTime"`
}

// HPAs ≈ kubectl get hpa
func (c *kubeClient) HPAs(ctx context.Context) ([]hpaView, error) {
	var list struct {
		Items []k8sHPA `json:"items"`
	}
	if err := c.do(ctx, "GET", c.nsPath("autoscaling/v2", "horizontalpodautoscalers"), "", nil, &list); err != nil {
		return nil, err
	}
	out := []hpaView{}
	for _, h := range list.Items {
		v := hpaView{
			Name:          h.Metadata.Name,
			Target:        h.Spec.ScaleTargetRef.Name,
			Min:           1,
			Max:           h.Spec.MaxReplicas,
			Current:       h.Status.CurrentReplicas,
			Desired:       h.Status.DesiredReplicas,
			Metrics:       []string{},
			LastScaleTime: h.Status.LastScaleTime,
		}
		if h.Spec.MinReplicas != nil {
			v.Min = *h.Spec.MinReplicas
		}
		for i, m := range h.Spec.Metrics {
			name, target := metricTarget(m)
			current := "?"
			if i < len(h.Status.CurrentMetrics) {
				_, current = metricCurrent(h.Status.CurrentMetrics[i])
			}
			v.Metrics = append(v.Metrics, fmt.Sprintf("%s: %s / %s", name, current, target))
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b hpaView) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// metricBody возвращает имя метрики и вложенный объект нужного типа
// (resource / external / pods / object) из спецификации HPA.
func metricBody(m map[string]any) (string, map[string]any) {
	typ, _ := m["type"].(string)
	if typ == "" {
		return "?", nil
	}
	body, _ := m[strings.ToLower(typ[:1])+typ[1:]].(map[string]any)
	if body == nil {
		return typ, nil
	}
	if name, ok := body["name"].(string); ok { // Resource: {"name": "cpu"}
		return name, body
	}
	if metric, ok := body["metric"].(map[string]any); ok { // External/Pods: {"metric": {"name": ...}}
		if name, ok := metric["name"].(string); ok {
			return name, body
		}
	}
	return typ, body
}

func metricTarget(m map[string]any) (string, string) {
	name, body := metricBody(m)
	t, _ := body["target"].(map[string]any)
	return name, formatMetricValue(t)
}

func metricCurrent(m map[string]any) (string, string) {
	name, body := metricBody(m)
	cur, _ := body["current"].(map[string]any)
	return name, formatMetricValue(cur)
}

func formatMetricValue(v map[string]any) string {
	switch {
	case v == nil:
		return "?"
	case v["averageUtilization"] != nil:
		return fmt.Sprintf("%v%%", v["averageUtilization"])
	case v["averageValue"] != nil:
		return humanQuantity(fmt.Sprint(v["averageValue"])) + " (avg)"
	case v["value"] != nil:
		return humanQuantity(fmt.Sprint(v["value"]))
	}
	return "?"
}

// humanQuantity: "23500m" -> "23.5", "10" -> "10".
func humanQuantity(q string) string {
	if m, ok := strings.CutSuffix(q, "m"); ok {
		if n, err := strconv.ParseFloat(m, 64); err == nil {
			return strconv.FormatFloat(n/1000, 'f', -1, 64)
		}
	}
	return q
}
