KEY_DIR := keys
SERVER_KEY := cmd/sudogate-server/laptop.key
CLIENT_PUB := cmd/sudogate-client/laptop.pub

keys:
	openssl genpkey -algorithm ed25519 -out $(KEY_DIR)/laptop.key
	openssl pkey -in $(KEY_DIR)/laptop.key -pubout -out $(KEY_DIR)/laptop.pub

inject:
	cp $(KEY_DIR)/laptop.key $(SERVER_KEY)
	cp $(KEY_DIR)/laptop.pub $(CLIENT_PUB)

build: inject
	go build -o build/sudogate-server ./cmd/sudogate-server
	go build -o build/sudogate-client ./cmd/sudogate-client
	go build -o build/sudogate-tui ./cmd/sudogate-tui

test:
	go test ./...

install-plugin:
	mkdir -p ~/.config/omarchy/plugins/tomaswade.sudogate
	cp -r plugin/manifest.json plugin/omarchy ~/.config/omarchy/plugins/tomaswade.sudogate/
	python3 scripts/omarchy-register.py
	omarchy-restart-shell

install-server: build
	install -m 0755 build/sudogate-server ~/.local/bin/
	mkdir -p ~/.config/systemd/user
	install -m 0644 packaging/sudogate.service ~/.config/systemd/user/
	systemctl --user daemon-reload
	systemctl --user enable --now sudogate.service

# 转发通道由 server 内嵌管理（forward.conf + ctl 热生效）。
# make install-forward HOST=<ssh-target>：server 在线则热添加（被拒绝则报错中止），
# ctl 不可达（exit 3）才落盘配置待 server 启动生效。
# HOST 支持 host 或 host:/abs/path/sock（远端 uid 非 1000 时覆盖路径）。
install-forward: build
	@test -n "$(HOST)" || (echo "用法: make install-forward HOST=<ssh目标>|<host:/path/sock>"; exit 2)
	@BIN=build/sudogate-server; [ -x $$BIN ] || BIN=$$HOME/.local/bin/sudogate-server; \
	if out=$$($$BIN forward add $(HOST) 2>&1); then \
		echo "已热添加转发: $(HOST)"; \
	elif [ $$? -eq 3 ]; then \
		h=$${HOST%%:*}; mkdir -p ~/.config/sudogate; \
		if grep -qE "^$$h(:|$$)" ~/.config/sudogate/forward.conf 2>/dev/null; then \
			echo "配置已含 $$h 的行（改路径请编辑 forward.conf）"; \
		else \
			echo '$(HOST)' >> ~/.config/sudogate/forward.conf; \
			echo "server 未在线，已写入配置（启动后生效）: $(HOST)"; \
		fi; \
	else \
		echo "forward add 被拒绝: $$out" >&2; exit 1; \
	fi

install-client: build
	install -m 0755 build/sudogate-client ~/.local/bin/

.PHONY: keys inject build test install-plugin install-server install-client install-forward
