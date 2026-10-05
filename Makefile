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

install-client: build
	install -m 0755 build/sudogate-client ~/.local/bin/

.PHONY: keys inject build test install-plugin install-server install-client
