module model-proxy

go 1.26.4

require (
	// Pinned to the upstream main-branch commit that added native go1.27
	// support (v1.15.2 silently falls back to encoding/json there). Replace
	// with the tagged v1.15.3 release once it is out.
	github.com/bytedance/sonic v1.15.3-0.20260730064818-2a36d6da63e2
	github.com/mattn/go-isatty v0.0.20
	github.com/zalando/go-keyring v0.2.8
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.53.0
)

require golang.org/x/sys v0.44.0

require (
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic/loader v0.5.2 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	golang.org/x/arch v0.0.0-20210923205945-b76863e36670 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
