APPLICATION := kubeling
CHART ?= $(APPLICATION)
CHART_DIR ?= charts/$(CHART)

IMAGE_REPOSITORY := git.example.com/platform/$(APPLICATION)
IMAGE_TAG := latest

VALUES ?=
ifneq ($(VALUES),)
	VALUES_FLAG := --values=$(VALUES)
endif

.PHONY: all build deploy

all: build deploy
	@true

build:
	docker buildx build --platform=linux/amd64,linux/arm64 --tag=$(IMAGE_REPOSITORY):$(IMAGE_TAG) --push .

deploy:
	helm upgrade --install $(APPLICATION) $(CHART_DIR) $(VALUES_FLAG) --debug
