IMAGE ?= quay.io/pacevedo/shift-week-notes:latest
NAMESPACE = shift-week-demo
NODE_IP ?= $(error Set NODE_IP to the MicroShift/SNO node IP, e.g. make test NODE_IP=192.168.1.10)
MANIFESTS_DIR = $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

.PHONY: build push deploy undeploy test

build:
	podman build -t $(IMAGE) $(MANIFESTS_DIR)

push: build
	podman push $(IMAGE)

deploy:
	oc apply -f $(MANIFESTS_DIR)
	oc wait --for=condition=ready pod -l app=demo-app -n $(NAMESPACE) --timeout=120s
	@echo ""
	@echo "App is ready at: https://$$(oc get route demo-app -n $(NAMESPACE) -o jsonpath='{.spec.host}')"

undeploy:
	oc delete -f $(MANIFESTS_DIR) --ignore-not-found

test:
	@ROUTE=$$(oc get route demo-app -n $(NAMESPACE) -o jsonpath='{.spec.host}'); \
	echo "GET:"; \
	curl -sk --connect-to "$$ROUTE:443:$(NODE_IP):443" https://$$ROUTE | python3 -m json.tool; \
	echo ""; \
	echo "POST:"; \
	curl -sk --connect-to "$$ROUTE:443:$(NODE_IP):443" -X POST -d "test note from make" https://$$ROUTE; \
	echo ""; \
	echo "GET after POST:"; \
	curl -sk --connect-to "$$ROUTE:443:$(NODE_IP):443" https://$$ROUTE | python3 -m json.tool
