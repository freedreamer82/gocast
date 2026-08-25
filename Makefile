BIN := gocast
UUID := gocast@local
EXTDIR := $(HOME)/.local/share/gnome-shell/extensions/$(UUID)

export GO111MODULE := on

# The commit is stamped into the binary so that both ends can say exactly what
# they are running: the receiver announces it, the sender compares. Falls back
# to whatever Go records from the repository when git is not around.
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -X gocast/internal/version.Commit=$(COMMIT)

.PHONY: build pi64 pi32 install install-extension test clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/gocast

# No cgo: cross-compiling needs no external toolchain.
pi64:
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN)-arm64 ./cmd/gocast

pi32:
	GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" -o $(BIN)-armv7 ./cmd/gocast

# /usr/local/bin rather than ~/bin: gnome-shell does not have ~/bin on its
# PATH, and the extension launches the binary by name.
install: build
	sudo install -m755 $(BIN) /usr/local/bin/$(BIN)

install-extension:
	mkdir -p $(EXTDIR)
	cp gnome-extension/metadata.json gnome-extension/extension.js gnome-extension/stylesheet.css $(EXTDIR)/
	@echo "Under Wayland this needs a logout/login, then: gnome-extensions enable $(UUID)"

test:
	go vet ./...
	go test ./...
	go build ./...

clean:
	rm -f $(BIN) $(BIN)-arm64 $(BIN)-armv7
