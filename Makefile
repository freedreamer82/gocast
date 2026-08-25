BIN := gocast
UUID := gocast@local
EXTDIR := $(HOME)/.local/share/gnome-shell/extensions/$(UUID)

export GO111MODULE := on

.PHONY: build pi64 pi32 install install-extension test clean

build:
	go build -o $(BIN) ./cmd/gocast

# Niente cgo: la cross-compilazione non richiede toolchain esterne.
pi64:
	GOOS=linux GOARCH=arm64 go build -o $(BIN)-arm64 ./cmd/gocast

pi32:
	GOOS=linux GOARCH=arm GOARM=7 go build -o $(BIN)-armv7 ./cmd/gocast

# /usr/local/bin e non ~/bin: gnome-shell non ha ~/bin nel PATH, e
# l'estensione lancia il binario per nome.
install: build
	sudo install -m755 $(BIN) /usr/local/bin/$(BIN)

install-extension:
	mkdir -p $(EXTDIR)
	cp gnome-extension/metadata.json gnome-extension/extension.js gnome-extension/stylesheet.css $(EXTDIR)/
	@echo "Su Wayland serve un logout/login, poi: gnome-extensions enable $(UUID)"

test:
	go vet ./...
	go test ./...
	go build ./...

clean:
	rm -f $(BIN) $(BIN)-arm64 $(BIN)-armv7
