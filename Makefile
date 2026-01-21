# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get

VERSION=1.0

# Output helpers
# --------------

TASK_DONE = echo "✓  $@ done"
TASK_BUILD = echo "🛠️  $@ done"

export GITHUB_RUN_ID ?= 0
export GITHUB_SHA ?= $(shell git rev-list -1 HEAD)
export DATE = $(shell date -u '+%Y%m%d')

all: test build

deps:
	go get -v  ./...

build:
	tinygo build -o build.uf2 -target=pico2 -ldflags="-X 'openenterprise/bindicator/version.Version=$(VERSION)' -X 'openenterprise/bindicator/version.GitSHA=$(GITHUB_SHA)' -X 'openenterprise/bindicator/version.BuildDate=$(DATE)'" -scheduler=tasks .
	@$(TASK_DONE)

test: 
	@$(GOTEST) -v ./...
	@$(TASK_DONE)

cli:
	go build -o bindicator-cli ./cmd/cli
	@$(TASK_DONE)

clean:
	@$(GOCLEAN)
	@rm -f ./bootstrap ./bindicator-cli ./build.uf2
	@$(TASK_DONE)

clean-cache:
	@rm -rf ~/.cache/tinygo/thinlto/
	@$(TASK_DONE)

# Partition table management (requires picotool 2.2+)
partition-table:
	picotool partition create partitions/bindicator.json partitions/pt.uf2
	@$(TASK_DONE)

# Flash partition table to device (device must be in BOOTSEL mode)
flash-partition:
	picotool load partitions/pt.uf2
	picotool reboot -u
	@echo "Device rebooted to BOOTSEL mode, ready for application"
	@$(TASK_DONE)

# Flash application to partitioned device (device must be in BOOTSEL mode)
flash:
	picotool load -x build.uf2
	@$(TASK_DONE)

# Full setup: partition table + application (device must be in BOOTSEL mode)
flash-full: partition-table
	picotool load partitions/pt.uf2
	picotool reboot -u
	@sleep 2
	picotool load -x build.uf2
	@$(TASK_DONE)

# Show partition info (device must be in BOOTSEL mode)
partition-info:
	picotool partition info
