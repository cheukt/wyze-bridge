# Pinned to match docker/Dockerfile's GO2RTC_VERSION.
GO2RTC_VERSION ?= 1.9.14

.PHONY: build run

build:
	go build -o wyze-headless ./cmd/wyze-headless

run: go2rtc
	./wyze-headless

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
