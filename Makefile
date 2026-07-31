PREFIX ?= /usr/local
DESTDIR ?=
BINDIR ?= $(PREFIX)/bin
DATADIR ?= $(PREFIX)/share
DOCDIR ?= $(DATADIR)/doc/murmur
INSTALL_OSD ?= 0

DOC_FILES := \
	acceptance.md \
	audio.md \
	cli-result.schema.json \
	cli.md \
	cleanup-benchmark.md \
	configuration.md \
	desktop-validation.md \
	doctor.md \
	implementation-status.md \
	ipc.md \
	markdown.md \
	osd-protocol.md \
	production-migration.md \
	providers.md \
	session.md \
	sinks.md \
	text-pipeline.md
EXAMPLE_FILES := config.minimal.toml config.full.toml

.PHONY: build test lint clean install uninstall installcheck check-install-vars

build:
	mkdir -p build
	go build -trimpath -o build/murmur ./cmd/murmur

test:
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...

clean:
	rm -rf build

check-install-vars:
	@case "$(PREFIX)" in /*) ;; *) printf '%s\n' 'PREFIX doit être un chemin absolu' >&2; exit 2;; esac
	@case "$(DESTDIR)" in ''|/*) ;; *) printf '%s\n' 'DESTDIR doit être vide ou absolu' >&2; exit 2;; esac
	@case "$(BINDIR)" in /*) ;; *) printf '%s\n' 'BINDIR doit être un chemin absolu' >&2; exit 2;; esac
	@case "$(DATADIR)" in /*) ;; *) printf '%s\n' 'DATADIR doit être un chemin absolu' >&2; exit 2;; esac
	@case "$(DOCDIR)" in /*) ;; *) printf '%s\n' 'DOCDIR doit être un chemin absolu' >&2; exit 2;; esac
	@case "$(INSTALL_OSD)" in 0|1) ;; *) printf '%s\n' 'INSTALL_OSD doit valoir 0 ou 1' >&2; exit 2;; esac

install: check-install-vars build
	install -d "$(DESTDIR)$(BINDIR)"
	install -m 0755 build/murmur "$(DESTDIR)$(BINDIR)/murmur"
	install -d "$(DESTDIR)$(DOCDIR)/docs" "$(DESTDIR)$(DOCDIR)/examples"
	install -m 0644 README.md "$(DESTDIR)$(DOCDIR)/README.md"
	install -m 0644 $(addprefix docs/,$(DOC_FILES)) "$(DESTDIR)$(DOCDIR)/docs/"
	install -m 0644 $(addprefix examples/,$(EXAMPLE_FILES)) "$(DESTDIR)$(DOCDIR)/examples/"
	@if [ "$(INSTALL_OSD)" = 1 ]; then \
		install -m 0755 helpers/osd-gtk/murmur-osd.py "$(DESTDIR)$(BINDIR)/murmur-osd"; \
	fi

uninstall: check-install-vars
	rm -f "$(DESTDIR)$(BINDIR)/murmur" "$(DESTDIR)$(BINDIR)/murmur-osd"
	rm -f "$(DESTDIR)$(DOCDIR)/README.md"
	@for file in $(DOC_FILES); do rm -f "$(DESTDIR)$(DOCDIR)/docs/$$file"; done
	@for file in $(EXAMPLE_FILES); do rm -f "$(DESTDIR)$(DOCDIR)/examples/$$file"; done
	@rmdir "$(DESTDIR)$(DOCDIR)/docs" "$(DESTDIR)$(DOCDIR)/examples" "$(DESTDIR)$(DOCDIR)" 2>/dev/null || true

installcheck: build
	@root="$$(mktemp -d)"; \
	trap 'rm -rf "$$root"' EXIT HUP INT TERM; \
	$(MAKE) --no-print-directory install DESTDIR="$$root" PREFIX=/usr INSTALL_OSD=1; \
	test -x "$$root/usr/bin/murmur"; \
	test -x "$$root/usr/bin/murmur-osd"; \
	test -f "$$root/usr/share/doc/murmur/README.md"; \
	test -f "$$root/usr/share/doc/murmur/docs/ipc.md"; \
	test -f "$$root/usr/share/doc/murmur/docs/desktop-validation.md"; \
	test -f "$$root/usr/share/doc/murmur/examples/config.minimal.toml"; \
	(cd "$$root" && "$$root/usr/bin/murmur" help >/dev/null); \
	$(MAKE) --no-print-directory uninstall DESTDIR="$$root" PREFIX=/usr; \
	test ! -e "$$root/usr/bin/murmur"; \
	test ! -e "$$root/usr/bin/murmur-osd"; \
	test ! -e "$$root/usr/share/doc/murmur/README.md"
