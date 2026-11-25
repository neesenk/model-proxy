module model-proxy

go 1.27.4

require (
	// Pinned to the upstream main-branch commit that added native go1.27
	// support (v1.15.2 silently falls back to encoding/json there). Replace
	// with the tagged v1.15.3 release once it is out.
	github.com/bytedance/sonic v1.15.3-0.20260730064818-2a36d6da63e2
	github.com/klauspost/compress v1.20.5
	github.com/mattn/go-isatty v0.0.20
	github.com/zalando/go-keyring v0.2.8
	golang.org/x/sys v0.49.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.53.0
)

require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic/loader v0.5.2 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/subcommands v1.2.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kisielk/errcheck v1.20.0 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/loov/goda v0.10.2 // indirect
	github.com/mibk/dupl v1.1.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/uudashr/gocognit v1.3.0 // indirect
	golang.org/x/arch v0.0.0-20210923205945-b76863e36670 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/image v0.47.0 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/telemetry v0.0.0-20261009151841-97e8ea77aa2a // indirect
	golang.org/x/tools v0.52.0 // indirect
	golang.org/x/vuln v1.8.0 // indirect
	honnef.co/go/tools v0.8.1 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	mvdan.cc/unparam v0.0.0-20261102141913-394cd0a56cd5 // indirect
)

tool (
	github.com/kisielk/errcheck
	github.com/loov/goda
	github.com/mibk/dupl
	github.com/uudashr/gocognit/cmd/gocognit
	golang.org/x/tools/cmd/deadcode
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
	mvdan.cc/unparam
)
