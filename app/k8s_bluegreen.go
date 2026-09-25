package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Blue/green: две полноценные версии приложения работают одновременно, а
// основной Service смотрит только на одну из них. Переключение — смена selector'а
// Service: весь трафик уходит на другую версию разом (в отличие от rolling
// update), а откат — такое же мгновенное переключение обратно.
//
//	blue  = основной Deployment kube-demo        (selector app=kube-demo)
//	green = k8s/extras/bluegreen/green.yaml       (selector app=kube-demo-green)

const (
	appService      = "kube-demo"
	greenDeployment = "kube-demo-green"
)

var errGreenNotReady = errors.New("в green нет ни одного Ready-пода: переключать трафик некуда")

type slotView struct {
	Deployment string `json:"deployment"`
	Desired    int32  `json:"desired"`
	Ready      int32  `json:"ready"`
}

type blueGreenView struct {
	Active   string            `json:"active"` // blue | green | "" (selector не похож ни на один)
	Selector map[string]string `json:"selector"`
	Blue     slotView          `json:"blue"`
	Green    *slotView         `json:"green"` // nil — green не развёрнут
}

func (c *kubeClient) slot(ctx context.Context, deployment string) (*slotView, error) {
	var d k8sDeployment
	if err := c.do(ctx, "GET", c.nsPath("apps/v1", "deployments/"+deployment), "", nil, &d); err != nil {
		return nil, err
	}
	return &slotView{Deployment: deployment, Desired: d.Spec.Replicas, Ready: d.Status.ReadyReplicas}, nil
}

// BlueGreen ≈ kubectl get svc kube-demo -o jsonpath='{.spec.selector}' + get deploy
func (c *kubeClient) BlueGreen(ctx context.Context) (blueGreenView, error) {
	var svc struct {
		Spec struct {
			Selector map[string]string `json:"selector"`
		} `json:"spec"`
	}
	v := blueGreenView{}
	if err := c.do(ctx, "GET", c.nsPath("", "services/"+appService), "", nil, &svc); err != nil {
		return v, err
	}
	v.Selector = svc.Spec.Selector
	switch svc.Spec.Selector["app"] {
	case c.deployment:
		v.Active = "blue"
	case greenDeployment:
		v.Active = "green"
	}

	blue, err := c.slot(ctx, c.deployment)
	if err != nil {
		return v, err
	}
	v.Blue = *blue
	green, err := c.slot(ctx, greenDeployment)
	switch {
	case isNotFound(err):
	case err != nil:
		return v, err
	default:
		v.Green = green
	}
	return v, nil
}

// SwitchTraffic ≈ kubectl patch svc kube-demo -p '{"spec":{"selector":{"app":"kube-demo-green"}}}'
// Перед переключением проверяем, что в целевой версии есть Ready-поды.
func (c *kubeClient) SwitchTraffic(ctx context.Context, slot string) (string, error) {
	target := map[string]string{"blue": c.deployment, "green": greenDeployment}[slot]
	if target == "" {
		return "", fmt.Errorf("неизвестный слот %q: blue | green", slot)
	}
	sv, err := c.slot(ctx, target)
	if err != nil {
		return "", err
	}
	if sv.Ready == 0 {
		if slot == "green" {
			return "", errGreenNotReady
		}
		return "", fmt.Errorf("в %s нет ни одного Ready-пода: переключать трафик некуда", target)
	}
	patch := map[string]any{"spec": map[string]any{"selector": map[string]string{"app": target}}}
	return target, c.do(ctx, "PATCH", c.nsPath("", "services/"+appService), "application/merge-patch+json", patch, nil)
}

// POST /api/k8s/bluegreen/{slot}
func (s *server) handleSwitchTraffic(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	slot := r.PathValue("slot")
	target, err := s.kube.SwitchTraffic(r.Context(), slot)
	var apiErr *apiError
	switch {
	case err == nil:
	case errors.As(err, &apiErr):
		writeKubeError(w, err)
		return
	default: // не готова целевая версия или неизвестный слот — ошибка клиента
		writeError(w, http.StatusConflict, err)
		return
	}
	s.metrics.action("switch_" + slot)
	writeJSON(w, http.StatusOK, map[string]any{
		"servedBy": s.cfg.PodName,
		"active":   slot,
		"command":  fmt.Sprintf(`kubectl patch svc %s -p '{"spec":{"selector":{"app":"%s"}}}'`, appService, target),
	})
}
