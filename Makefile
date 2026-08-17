CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen
KIND_CLUSTER      ?= doblura-e2e
KIND_CHART_CLUSTER ?= doblura-e2e-chart

.PHONY: generate build test chart-sync lint-chart verify-image e2e e2e-chart e2e-real e2e-clean all
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
	@for c in logLevel=nonsense logFormat=xml replicaCount=-1 metrics.port=99999; do \
		if helm template t charts/doblura --set $$c >/dev/null 2>&1; then \
			echo "FAIL: the schema accepted --set $$c"; exit 1; \
		fi; done
	@echo "  values.schema.json rejects typos"

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
