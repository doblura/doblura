CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen
KIND_CLUSTER      ?= doblura-e2e
KIND_CHART_CLUSTER ?= doblura-e2e-chart
# Built locally and side-loaded into kind: e2e-quota needs a real image to serve
# admission from, and the published one does not exist yet.
WEBHOOK_IMAGE      ?= doblura:e2e

# ── the base image ───────────────────────────────────────────────────────────
#
# One per Odoo major, built from the official image plus the tools this operator
# needs. See images/odoo/Dockerfile for why each line is there, and DECISIONS.md 11
# for why it carries no modules.
#
# The versions are DECISIONS.md 15: the three Odoo supports, plus the one in its
# grace year — because doblura exists to rehearse the migration off an old version,
# and dropping it the week Odoo does removes the tool from the people who need it.
ODOO_VERSIONS ?= 19.0 18.0 17.0 16.0
IMAGE_PREFIX  ?= doblura/odoo

.PHONY: images
images: ## Build the Doblura base image for every supported Odoo
	@for v in $(ODOO_VERSIONS); do \
		echo "  building $(IMAGE_PREFIX):$$v"; \
		docker build --build-arg ODOO_VERSION=$$v -t $(IMAGE_PREFIX):$$v images/odoo || exit 1; \
	done

.PHONY: image
image: ## Build one: make image ODOO_VERSION=18.0
	docker build --build-arg ODOO_VERSION=$(ODOO_VERSION) -t $(IMAGE_PREFIX):$(ODOO_VERSION) images/odoo

.PHONY: generate build test chart-sync lint-chart verify-image e2e e2e-chart e2e-quota e2e-real e2e-clean all
all: generate build test verify-licence lint-chart

# The licence boundary is a build check, not a paragraph in a document: api/ is
# Apache-2.0 so proprietary code can import it, and one import of internal/ voids
# that silently, because it still compiles.
verify-licence:
	@./hack/verify-licence-boundary.sh

generate:
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=doblura-manager paths=./internal/... output:rbac:artifacts:config=config/rbac
	$(CONTROLLER_GEN) webhook paths=./internal/... output:webhook:artifacts:config=config/webhook

build:
	go build ./...
	go vet ./...

test:
	go test ./... -race

## The chart used to hand-maintain copies of generated content (the CRDs and
## the RBAC rules) and both diverged silently. This regenerates them.
chart-sync: generate
	@python3 hack/sync-chart.py

lint-chart: chart-sync
	helm lint charts/doblura
	helm template t charts/doblura >/dev/null
	@for c in logLevel=nonsense logFormat=xml replicaCount=-1 metrics.port=99999 \
	          webhook.port=99999 webhook.maxEnvironmentsPerCreator=0 webhook.enabled=maybe; do \
		if helm template t charts/doblura --set $$c >/dev/null 2>&1; then \
			echo "FAIL: the schema accepted --set $$c"; exit 1; \
		fi; done
	@echo "  values.schema.json rejects typos"
	@python3 hack/verify-webhook-chart.py

## verify-image: report what Doblura can drive with a candidate image.
## Usage: make verify-image IMAGE=odoo:19.0
IMAGE ?= odoo:19.0
verify-image:
	@docker run --rm -v "$(PWD)/hack/verify-image.sh:/verify.sh:ro" \
		--entrypoint sh $(IMAGE) /verify.sh

## verify-greenmask: validate the generated masking config with greenmask itself.
## Go tests assert on substrings; only the real tool catches a wrong schema.
verify-greenmask:
	./hack/verify-greenmask-config.sh

## e2e: install the CRDs into a kind cluster with kubectl and check that the
## guardrails reject what they should. If it has not run against an API server,
## it is not done.
e2e:
	kind create cluster --name $(KIND_CLUSTER) --wait 60s || true
	kind export kubeconfig --name $(KIND_CLUSTER)
	kubectl apply -f config/crd/
	kubectl apply --dry-run=server -f config/samples/
	./hack/e2e-guardrails.sh

## Chart e2e: a real helm install plus helm test.
##
## A SEPARATE cluster from `make e2e` on purpose: that target applies the CRDs
## with kubectl, and Helm refuses to adopt resources it does not own
## ("invalid ownership metadata"). Sharing a cluster makes this fail in a way
## that looks like a chart bug and is not.
e2e-chart: chart-sync
	kind create cluster --name $(KIND_CHART_CLUSTER) --wait 60s || true
	kind export kubeconfig --name $(KIND_CHART_CLUSTER)
	helm upgrade --install doblura charts/doblura \
		-n doblura-system --create-namespace --set replicaCount=0 --wait
	helm test doblura -n doblura-system

## e2e-quota: the quota webhook, against a real API server, with the operator
## actually serving admission.
##
## `make e2e` runs the CEL guardrails against the CRDs alone, and the quota is not
## a CEL rule: it needs a webhook, which needs an image, a Service and a CA. So
## this target builds the image, loads it into the chart cluster and installs the
## chart for real. Without it the quota section of e2e-guardrails.sh skips itself,
## which it says out loud.
##
## maxEnvironmentsPerCreator is set to 2 so the per-person limit can be reached in
## three creates instead of six.
e2e-quota: chart-sync
	docker build -t $(WEBHOOK_IMAGE) .
	kind create cluster --name $(KIND_CHART_CLUSTER) --wait 60s || true
	kind export kubeconfig --name $(KIND_CHART_CLUSTER)
	kind load docker-image $(WEBHOOK_IMAGE) --name $(KIND_CHART_CLUSTER)
	helm upgrade --install doblura charts/doblura \
		-n doblura-system --create-namespace \
		--set image.repository=doblura --set image.tag=e2e \
		--set image.pullPolicy=Never \
		--set webhook.maxEnvironmentsPerCreator=2 --wait
	kubectl -n doblura-system rollout status deploy/doblura --timeout=120s
	./hack/e2e-guardrails.sh

## e2e-real: the phase-0b gate. Builds the fixture image, seeds a real Odoo
## database, and runs an actual OdooRehearsal end to end.
e2e-real: chart-sync
	./hack/e2e/run.sh

## e2e-ocb: the same, against an image built from OCB instead of official Odoo.
## Slow: it clones OCB.
e2e-ocb:
	docker build -f hack/e2e/Dockerfile.ocb -t doblura-ocb-test:19.0 hack/e2e
	$(MAKE) verify-image IMAGE=doblura-ocb-test:19.0

e2e-clean:
	-kind delete cluster --name $(KIND_CLUSTER)
	-kind delete cluster --name $(KIND_CHART_CLUSTER)
