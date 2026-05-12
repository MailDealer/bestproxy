REGISTRY  ?= docker.io
USER      ?= youruser
IMAGE     := $(REGISTRY)/$(USER)/bestproxy
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

.PHONY: build push release

build:
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

push:
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest

release: build push
