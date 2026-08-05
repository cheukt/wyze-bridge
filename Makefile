# Pinned to match docker/Dockerfile's GO2RTC_VERSION.
GO2RTC_VERSION ?= 1.9.14

VERSION ?= dev

.PHONY: build run module module.tar.gz clean-module reload

build:
	go build -o wyze-headless ./cmd/wyze-headless

run: go2rtc
	./wyze-headless

# ── Viam module ──────────────────────────────────────────────────────────────
# Build the module binary. CGO-free for a portable, statically-linked entrypoint.
# The `no_cgo` tag drops gostream's cgo-only mediadevices audio driver (pulled in
# transitively by rdk/components/camera), which otherwise fails to link under
# CGO_ENABLED=0 with "undefined: malgo.AllocatedContext".
module: bin/viam-module

# Delegates to mise, which pins the Go toolchain (see mise.toml). VERSION is
# passed through; mise's build task defaults it to "dev".
bin/viam-module:
	VERSION=$(VERSION) mise run build

# Bundle the module entrypoint + go2rtc into module.tar.gz.
# go2rtc lives in bin/ next to the entrypoint so findGo2RTCBinary() locates it
# via os.Executable(). Built natively — Viam cloud build runs this on the
# target arch, so no cross-compilation is needed.
module.tar.gz: bin/viam-module go2rtc
	cp ./go2rtc bin/go2rtc
	chmod +x bin/viam-module bin/go2rtc
	tar -czf module.tar.gz bin/viam-module bin/go2rtc meta.json

clean-module:
	rm -rf bin module.tar.gz

# Cloud hot-reload the module to a running machine part. The viam CLI runs
# meta.json's build step and restarts the module on the part. Part id comes
# from .env.viam (gitignored — copy .env.viam.example).
reload:
	@[ -f .env.viam ] || { echo "missing .env.viam — copy .env.viam.example and set VIAM_PART_ID" >&2; exit 1; }
	@. ./.env.viam; \
	[ -n "$$VIAM_PART_ID" ] || { echo "VIAM_PART_ID not set in .env.viam" >&2; exit 1; }; \
	viam module reload --part-id $$VIAM_PART_ID

# Download the pinned go2rtc binary for the local platform, but only if
# ./go2rtc is missing (it's a file target, so make skips it once present).
# findGo2RTCBinary() checks ./go2rtc first.
go2rtc:
	@OS=$$(uname -s); ARCH=$$(uname -m); \
	case "$$ARCH" in arm64|aarch64) A=arm64;; x86_64|amd64) A=amd64;; *) A=$$ARCH;; esac; \
	echo "Downloading go2rtc $(GO2RTC_VERSION) ($$OS/$$A)..."; \
	case "$$OS" in \
	  Darwin) \
	    curl -fsSL "https://github.com/AlexxIT/go2rtc/releases/download/v$(GO2RTC_VERSION)/go2rtc_mac_$$A.zip" -o /tmp/go2rtc.zip \
	      && unzip -o /tmp/go2rtc.zip -d . && rm -f /tmp/go2rtc.zip ;; \
	  Linux) \
	    curl -fsSL "https://github.com/AlexxIT/go2rtc/releases/download/v$(GO2RTC_VERSION)/go2rtc_linux_$$A" -o ./go2rtc ;; \
	  *) echo "unsupported OS: $$OS" >&2; exit 1 ;; \
	esac; \
	chmod +x ./go2rtc; \
	./go2rtc --version

test:
	mise run test

# Format in place, then lint. mise pins golangci-lint v2 (see mise.toml).
lint:
	mise run lint
