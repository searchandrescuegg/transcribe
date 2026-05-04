# Makefile
all: setup hooks

# requires `nvm use --lts` or `nvm use node`
.PHONY: setup
setup: 
	npm install -g @commitlint/config-conventional @commitlint/cli  


.PHONY: hooks
hooks:
	@git config --local core.hooksPath .githooks/

.PHONY: push-message
# Replays every .wav fixture in docker/s3ninja/audio/ as a synthetic Pulsar S3 event,
# in chronological order with a default 3s delay between each. Pass extra flags via ARGS
# (e.g. `make push-message ARGS='-delay 30s'`). NOTE: no leading "--" — Go's flag.Parse()
# treats it as a sentinel and would stop processing flags after it.
push-message:
	@./scripts/push-message.sh $(ARGS)