IMAGE   ?= kube-demo
VERSION ?= 1.0.0
CLUSTER ?= kube-demo
NS      ?= kube-demo
URL     ?= http://localhost:30080

.DEFAULT_GOAL := help
.PHONY: help up down test run build kind-up kind-down load deploy undeploy status logs watch \
        smoke zero-downtime persistence features v2 metrics-server hpa-on hpa-off \
        netpol-on netpol-off quota-on quota-off prometheus gateway-install gateway-on backup \
        node-down node-up keda-install rps-autoscale-on rps-autoscale-off \
        bluegreen-on bluegreen-switch bluegreen-off

help: ## Список команд
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

up: kind-up build load deploy ## Всё сразу: kind-кластер + сборка образа + деплой
	@printf "\n  Готово: %s\n\n" "$(URL)"

down: kind-down ## Удалить кластер целиком

# ---------- приложение ----------

test: ## Тесты Go
	cd app && go vet ./... && go test -race ./...

run: ## Запустить локально без Kubernetes (хранилище в памяти) на :8080
	cd app && go run .

build: ## Собрать Docker-образ $(IMAGE):$(VERSION)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) app

# ---------- кластер ----------

kind-up: ## Создать kind-кластер (1 control-plane + 2 worker)
	@kind get clusters 2>/dev/null | grep -qx $(CLUSTER) || kind create cluster --config kind-config.yaml --name $(CLUSTER)

kind-down: ## Удалить kind-кластер
	kind delete cluster --name $(CLUSTER)

load: ## Загрузить образ в kind (реестр не нужен)
	kind load docker-image $(IMAGE):$(VERSION) --name $(CLUSTER)

deploy: ## kubectl apply -k k8s/ и дождаться готовности
	kubectl apply -k k8s/
	kubectl -n $(NS) rollout status statefulset/postgres --timeout=180s
	kubectl -n $(NS) rollout status deployment/kube-demo --timeout=180s

undeploy: ## Удалить приложение (namespace целиком, вместе с данными Postgres)
	kubectl delete -k k8s/ --ignore-not-found

# ---------- наблюдение ----------

status: ## Поды, сервисы, endpoints, тома
	kubectl -n $(NS) get pods,svc,endpointslices,pvc -o wide

logs: ## Логи всех реплик разом
	kubectl -n $(NS) logs -l app=kube-demo -f --prefix --max-log-requests 20

RPS ?= 3

watch: ## Бесконечный curl: какой под отвечает (make watch RPS=10; Ctrl+C — стоп)
	@delay=$$(awk 'BEGIN { printf "%.3f", 1 / $(RPS) }'); \
	while true; do curl -s --max-time 2 $(URL)/ || echo "ERR: нет ответа"; sleep $$delay; done

# ---------- сценарии ----------

smoke: ## Smoke-тест: отвечает, балансирует, пишет в БД
	./scripts/smoke-test.sh $(URL)

zero-downtime: ## Rolling update под нагрузкой — ошибок быть не должно
	./scripts/zero-downtime-test.sh $(URL)

persistence: ## Scale через приложение + убить поды и базу — данные должны остаться
	./scripts/persistence-test.sh $(URL)

v2: ## Собрать образ 2.0.0 и выкатить его rolling update'ом
	$(MAKE) build load VERSION=2.0.0
	kubectl -n $(NS) set image deployment/kube-demo app=$(IMAGE):2.0.0
	kubectl -n $(NS) annotate deployment/kube-demo kubernetes.io/change-cause="image $(IMAGE):2.0.0" --overwrite
	kubectl -n $(NS) rollout status deployment/kube-demo

# Отказ ноды. docker pause замораживает все процессы ноды kind (и kubelet тоже):
# для кластера она просто перестаёт отвечать, как при зависании или обрыве сети.
# unpause возвращает её с тем же IP — в отличие от docker stop/start.
NODE ?= kube-demo-worker2

node-down: ## Уронить ноду kind (по умолчанию NODE=kube-demo-worker2)
	docker pause $(NODE)
	@echo "Нода $(NODE) заморожена. Смотрите карту кластера в UI и: kubectl get nodes,pods -o wide -w"

node-up: ## Вернуть ноду
	docker unpause $(NODE)
	kubectl wait --for=condition=Ready node/$(NODE) --timeout=120s

features: ## E2E всех «продвинутых» фич (или одной: make features F=oom)
	./scripts/features-test.sh $(or $(F),all)

netpol-on: ## NetworkPolicy: к базе только поды с меткой postgres-access=true
	kubectl apply -f k8s/extras/network-policy.yaml

netpol-off: ## Убрать NetworkPolicy
	kubectl delete -f k8s/extras/network-policy.yaml --ignore-not-found

quota-on: ## ResourceQuota + LimitRange на namespace
	kubectl apply -f k8s/extras/quota.yaml

quota-off: ## Убрать квоты
	kubectl delete -f k8s/extras/quota.yaml --ignore-not-found

backup: ## Запустить бэкап Postgres прямо сейчас (Job из CronJob)
	kubectl -n $(NS) create job backup-$$(date +%s) --from=cronjob/postgres-backup

prometheus: ## Prometheus с автопоиском подов -> http://localhost:30090
	kubectl apply -f k8s/extras/observability/prometheus.yaml
	kubectl -n $(NS) rollout status deployment/prometheus --timeout=180s

# Traefik 3.7 ждёт TLSRoute v1 — он есть в стандартном канале Gateway API начиная с v1.5
GATEWAY_API ?= v1.5.1
TRAEFIK_CHART ?= 41.6.0

gateway-install: ## CRD Gateway API + Traefik (helm) -> http://localhost:30081
	kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/$(GATEWAY_API)/standard-install.yaml
	helm repo add traefik https://traefik.github.io/charts
	helm upgrade --install traefik traefik/traefik --version $(TRAEFIK_CHART) -n traefik --create-namespace \
		-f k8s/extras/gateway/traefik-values.yaml
	kubectl -n traefik rollout status deployment/traefik --timeout=180s
	kubectl wait --for=condition=Programmed gateway/traefik-gateway -n traefik --timeout=120s

gateway-on: ## Canary-версия + HTTPRoute 80/20
	kubectl apply -f k8s/extras/gateway/canary.yaml -f k8s/extras/gateway/httproute.yaml
	kubectl -n $(NS) rollout status deployment/kube-demo-canary --timeout=180s

KEDA_CHART ?= 2.21.0

keda-install: ## KEDA (helm): автоскейлинг по внешним метрикам
	helm repo add kedacore https://kedacore.github.io/charts
	helm upgrade --install keda kedacore/keda --version $(KEDA_CHART) -n keda --create-namespace
	kubectl -n keda rollout status deployment/keda-operator --timeout=180s
	kubectl -n keda rollout status deployment/keda-operator-metrics-apiserver --timeout=180s

rps-autoscale-on: ## Автоскейлинг по RPS: 10 rps на реплику (нужны make prometheus keda-install)
	kubectl apply -f k8s/extras/autoscaling/keda-rps.yaml

rps-autoscale-off: ## Выключить автоскейлинг по RPS (реплики останутся как есть)
	kubectl delete -f k8s/extras/autoscaling/keda-rps.yaml --ignore-not-found

bluegreen-on: ## Развернуть green-версию рядом с blue (трафик пока на blue)
	kubectl apply -f k8s/extras/bluegreen/green.yaml
	kubectl -n $(NS) rollout status deployment/kube-demo-green --timeout=180s

SLOT ?= green

bluegreen-switch: ## Переключить трафик: make bluegreen-switch SLOT=green|blue
	kubectl -n $(NS) patch svc kube-demo -p '{"spec":{"selector":{"app":"$(if $(filter blue,$(SLOT)),kube-demo,kube-demo-green)"}}}'

bluegreen-off: ## Вернуть трафик на blue и удалить green
	kubectl -n $(NS) patch svc kube-demo -p '{"spec":{"selector":{"app":"kube-demo"}}}'
	kubectl delete -f k8s/extras/bluegreen/green.yaml --ignore-not-found

metrics-server: ## Поставить metrics-server в kind (нужен для HPA и kubectl top)
	kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
	kubectl -n kube-system patch deployment metrics-server --type=json \
		-p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
	kubectl -n kube-system rollout status deployment/metrics-server --timeout=180s

hpa-on: ## Включить HPA и генератор нагрузки
	kubectl apply -f k8s/extras/hpa.yaml -f k8s/extras/load-generator.yaml

hpa-off: ## Выключить генератор нагрузки (HPA сам уменьшит число подов)
	kubectl delete -f k8s/extras/load-generator.yaml --ignore-not-found
