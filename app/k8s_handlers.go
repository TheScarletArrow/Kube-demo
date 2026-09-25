package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const (
	minReplicas = 1 // 0 нельзя: иначе некому будет обработать запрос «верни как было»
	maxReplicas = 10

	minCPUMilli = 100
	maxCPUMilli = 2000

	appContainer  = "app"
	backupCronJob = "postgres-backup"
)

func (s *server) kubeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/k8s/state", s.handleClusterState)
	mux.HandleFunc("PUT /api/k8s/replicas", s.handleScale)
	mux.HandleFunc("POST /api/k8s/restart", s.handleRolloutRestart)
	mux.HandleFunc("DELETE /api/k8s/pods/{name}", s.handleDeletePod)
	mux.HandleFunc("PUT /api/k8s/pods/{name}/cpu", s.handleResizeCPU)
	mux.HandleFunc("POST /api/k8s/break", s.handleBreakRelease)
	mux.HandleFunc("POST /api/k8s/undo", s.handleRolloutUndo)
	mux.HandleFunc("POST /api/k8s/backup", s.handleRunBackup)
	mux.HandleFunc("POST /api/k8s/nodes/{name}/{action}", s.handleNodeAction)

	mux.HandleFunc("POST /api/snapshots", s.handleCreateSnapshot)
	mux.HandleFunc("GET /api/snapshots/{id}", s.handleCheckSnapshot)
}

func (s *server) requireKube(w http.ResponseWriter) bool {
	if s.kube == nil {
		writeError(w, http.StatusNotImplemented,
			errors.New("управление кластером доступно только внутри Kubernetes"))
		return false
	}
	return true
}

// writeKubeError превращает ошибку Kubernetes API в понятный ответ.
func writeKubeError(w http.ResponseWriter, err error) {
	var apiErr *apiError
	switch {
	case errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden:
		writeError(w, http.StatusForbidden,
			fmt.Errorf("RBAC не разрешает это действие (см. k8s/app/rbac.yaml): %s", apiErr.Message))
	case errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound:
		writeError(w, http.StatusNotFound, errors.New(apiErr.Message))
	default:
		writeError(w, http.StatusBadGateway, err)
	}
}

// GET /api/k8s/state ≈ kubectl get deployment,replicasets,pods
func (s *server) handleClusterState(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	st, err := s.kube.State(r.Context())
	if err != nil {
		writeKubeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servedBy": s.cfg.PodName, "state": st})
}

// PUT /api/k8s/replicas {"replicas": 5} ≈ kubectl scale deployment/kube-demo --replicas=5
func (s *server) handleScale(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	var in struct {
		Replicas int32 `json:"replicas"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, errors.New(`ожидается JSON вида {"replicas": 3}`))
		return
	}
	if in.Replicas < minReplicas || in.Replicas > maxReplicas {
		writeError(w, http.StatusBadRequest, fmt.Errorf("replicas должно быть от %d до %d", minReplicas, maxReplicas))
		return
	}
	if err := s.kube.Scale(r.Context(), in.Replicas); err != nil {
		writeKubeError(w, err)
		return
	}
	cmd := fmt.Sprintf("kubectl scale deployment/%s --replicas=%d", s.kube.deployment, in.Replicas)
	slog.Info("scaled via API", "replicas", in.Replicas)
	s.metrics.action("scale")
	writeJSON(w, http.StatusOK, map[string]any{"servedBy": s.cfg.PodName, "replicas": in.Replicas, "command": cmd})
}

// POST /api/k8s/restart ≈ kubectl rollout restart deployment/kube-demo
func (s *server) handleRolloutRestart(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	if err := s.kube.RolloutRestart(r.Context()); err != nil {
		writeKubeError(w, err)
		return
	}
	slog.Info("rollout restart via API")
	s.metrics.action("rollout_restart")
	writeJSON(w, http.StatusOK, map[string]any{
		"servedBy": s.cfg.PodName,
		"command":  "kubectl rollout restart deployment/" + s.kube.deployment,
	})
}

// DELETE /api/k8s/pods/{name} ≈ kubectl delete pod <name>
// Удалять разрешаем только поды этой демки (приложение и Postgres).
func (s *server) handleDeletePod(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	name := r.PathValue("name")
	pod, err := s.kube.GetPod(r.Context(), name)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	if app := pod.Metadata.Labels["app"]; app != s.kube.deployment && app != "postgres" {
		writeError(w, http.StatusForbidden, fmt.Errorf("под %s не относится к демке (app=%q)", name, app))
		return
	}
	if err := s.kube.DeletePod(r.Context(), name); err != nil {
		writeKubeError(w, err)
		return
	}
	slog.Warn("pod deleted via API", "pod", name)
	s.metrics.action("delete_pod")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"servedBy": s.cfg.PodName,
		"deleted":  name,
		"command":  "kubectl delete pod " + name,
	})
}

// POST /api/k8s/break — выкатить сломанный релиз: поды нового ReplicaSet
// стартуют, но никогда не проходят readiness. Благодаря maxUnavailable: 0
// старые поды продолжают обслуживать трафик, а rollout через
// progressDeadlineSeconds получает статус ProgressDeadlineExceeded.
func (s *server) handleBreakRelease(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	if err := s.kube.BreakRelease(r.Context()); err != nil {
		writeKubeError(w, err)
		return
	}
	slog.Warn("broken release rolled out via API")
	s.metrics.action("break_release")
	writeJSON(w, http.StatusOK, map[string]any{
		"servedBy": s.cfg.PodName,
		"command": fmt.Sprintf("kubectl patch deployment/%s -p '{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"%s\":\"broken\"}}}}}'",
			s.kube.deployment, releaseAnnotation),
	})
}

// POST /api/k8s/undo ≈ kubectl rollout undo deployment/kube-demo
func (s *server) handleRolloutUndo(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	rev, err := s.kube.RolloutUndo(r.Context())
	if err != nil {
		writeKubeError(w, err)
		return
	}
	slog.Info("rollout undo via API", "toRevision", rev)
	s.metrics.action("rollout_undo")
	writeJSON(w, http.StatusOK, map[string]any{
		"servedBy":   s.cfg.PodName,
		"toRevision": rev,
		"command":    fmt.Sprintf("kubectl rollout undo deployment/%s   # -> шаблон ревизии %s", s.kube.deployment, rev),
	})
}

// PUT /api/k8s/pods/{name}/cpu {"limitMilli": 1000} ≈
// kubectl patch pod <name> --subresource resize -p '{"spec":{"containers":[{"name":"app","resources":...}]}}'
func (s *server) handleResizeCPU(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	var in struct {
		LimitMilli int `json:"limitMilli"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, errors.New(`ожидается JSON вида {"limitMilli": 1000}`))
		return
	}
	if in.LimitMilli < minCPUMilli || in.LimitMilli > maxCPUMilli {
		writeError(w, http.StatusBadRequest, fmt.Errorf("limitMilli должен быть от %d до %d", minCPUMilli, maxCPUMilli))
		return
	}

	name := r.PathValue("name")
	pod, err := s.kube.GetPod(r.Context(), name)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	if pod.Metadata.Labels["app"] != s.kube.deployment {
		writeError(w, http.StatusForbidden, fmt.Errorf("менять ресурсы можно только подам %s", s.kube.deployment))
		return
	}
	request := "50m"
	for _, c := range pod.Spec.Containers {
		if c.Name == appContainer && c.Resources.Requests["cpu"] != "" {
			request = c.Resources.Requests["cpu"]
		}
	}
	// requests не может быть больше limits.
	if req, err := parseMilliCPU(request); err == nil && req > in.LimitMilli {
		request = fmt.Sprintf("%dm", in.LimitMilli)
	}
	limit := fmt.Sprintf("%dm", in.LimitMilli)

	if err := s.kube.ResizePodCPU(r.Context(), name, appContainer, request, limit); err != nil {
		writeKubeError(w, err)
		return
	}
	slog.Info("pod resized in place via API", "pod", name, "cpuLimit", limit)
	s.metrics.action("resize")
	writeJSON(w, http.StatusOK, map[string]any{
		"servedBy": s.cfg.PodName,
		"pod":      name,
		"cpu":      request + "/" + limit,
		"command": fmt.Sprintf(`kubectl patch pod %s --subresource resize -p '{"spec":{"containers":[{"name":"%s","resources":{"limits":{"cpu":"%s"}}}]}}'`,
			name, appContainer, limit),
	})
}

// POST /api/k8s/backup ≈ kubectl create job --from=cronjob/postgres-backup
func (s *server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	job, err := s.kube.RunCronJobNow(r.Context(), backupCronJob)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	slog.Info("backup job started via API", "job", job)
	s.metrics.action("backup")
	writeJSON(w, http.StatusCreated, map[string]any{
		"servedBy": s.cfg.PodName,
		"job":      job,
		"command":  fmt.Sprintf("kubectl create job %s --from=cronjob/%s", job, backupCronJob),
	})
}

// POST /api/k8s/nodes/{name}/cordon|uncordon|drain ≈ kubectl cordon|uncordon|drain <node>
func (s *server) handleNodeAction(w http.ResponseWriter, r *http.Request) {
	if !s.requireKube(w) {
		return
	}
	node, action := r.PathValue("name"), r.PathValue("action")
	resp := map[string]any{"servedBy": s.cfg.PodName, "node": node}

	var err error
	switch action {
	case "cordon", "uncordon":
		err = s.kube.Cordon(r.Context(), node, action == "cordon")
		resp["command"] = "kubectl " + action + " " + node
	case "drain":
		var res drainResult
		res, err = s.kube.Drain(r.Context(), node)
		resp["result"] = res
		resp["command"] = fmt.Sprintf("kubectl drain %s --ignore-daemonsets   # только поды namespace %s: выселено %d, заблокировано %d",
			node, s.kube.namespace, len(res.Evicted), len(res.Blocked))
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("неизвестное действие %q: cordon | uncordon | drain", action))
		return
	}
	if err != nil {
		writeKubeError(w, err)
		return
	}
	slog.Warn("node action via API", "node", node, "action", action)
	s.metrics.action("node_" + action)
	writeJSON(w, http.StatusOK, resp)
}

// ---------- проверка сохранности данных ----------

// POST /api/snapshots — запомнить, сколько сообщений в гостевой книге и их контрольную сумму.
func (s *server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.store.CreateSnapshot(r.Context(), s.cfg.PodName)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	s.metrics.action("snapshot")
	writeJSON(w, http.StatusCreated, snap)
}

type snapshotCheck struct {
	Snapshot        Snapshot `json:"snapshot"`
	CheckedBy       string   `json:"checkedBy"`
	Storage         string   `json:"storage"`
	CurrentMessages int64    `json:"currentMessages"`
	CurrentDigest   string   `json:"currentDigest"`
	OK              bool     `json:"ok"`
	// Жив ли ещё под, сделавший снимок. nil — неизвестно (не в кластере).
	WriterAlive *bool `json:"writerAlive"`
}

// GET /api/snapshots/{id} — пересчитать контрольную сумму по текущим данным и сравнить со снимком.
func (s *server) handleCheckSnapshot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("id снимка должен быть числом"))
		return
	}

	snap, err := s.store.GetSnapshot(r.Context(), id)
	if errors.Is(err, ErrSnapshotNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":     fmt.Sprintf("снимок #%d не найден: данные потеряны (хранилище %s)", id, s.store.Kind()),
			"checkedBy": s.cfg.PodName,
			"storage":   s.store.Kind(),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}

	msgs, err := s.store.MessagesUpTo(r.Context(), snap.MaxID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	count, _, digest := digestMessages(msgs)

	writeJSON(w, http.StatusOK, snapshotCheck{
		Snapshot:        snap,
		CheckedBy:       s.cfg.PodName,
		Storage:         s.store.Kind(),
		CurrentMessages: count,
		CurrentDigest:   digest,
		OK:              count == snap.Messages && digest == snap.Digest,
		WriterAlive:     s.podAlive(r.Context(), snap.Pod),
	})
}

func (s *server) podAlive(ctx context.Context, name string) *bool {
	if s.kube == nil {
		return nil
	}
	if name == s.cfg.PodName {
		alive := true
		return &alive
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	pod, err := s.kube.GetPod(ctx, name)
	var apiErr *apiError
	switch {
	case errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound:
		alive := false
		return &alive
	case err != nil:
		return nil
	}
	alive := pod.Metadata.DeletionTimestamp == nil
	return &alive
}
