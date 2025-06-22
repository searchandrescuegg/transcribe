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
push-message:
	@./scripts/push-message.sh demo.m4a