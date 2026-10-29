module github.com/imksoo/routerd2

go 1.27.0

toolchain go1.27.3

require (
	github.com/facebook/time v0.0.0-20261028182048-956c8cce511a
	github.com/google/nftables v0.3.0
	github.com/google/uuid v1.6.0
	github.com/imksoo/routerd v0.3.1-0.20261023172044-1329ef54f296
	github.com/miekg/dns v1.1.72
	github.com/osrg/gobgp/v4 v4.7.0
	github.com/quic-go/quic-go v0.59.0
	github.com/vishvananda/netlink v1.3.1
	github.com/vishvananda/netns v0.0.5
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.60.0
	golang.org/x/sync v0.24.0
	golang.org/x/sys v0.49.0
	golang.org/x/term v0.47.0
	google.golang.org/grpc v1.80.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.50.0
)

require (
	github.com/dgryski/go-farm v0.0.0-20240924180020-3414d57e47da // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/eapache/channels v1.1.0 // indirect
	github.com/eapache/queue v1.1.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/gaissmai/bart v0.26.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/k-sone/critbitgo v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mdlayher/netlink v1.7.3-0.20250113171957-fbb4dce95f42 // indirect
	github.com/mdlayher/socket v0.5.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/orcaman/concurrent-map/v2 v2.0.1 // indirect
	github.com/pelletier/go-toml/v2 v2.2.3 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/sagikazarmark/locafero v0.7.0 // indirect
	github.com/segmentio/fasthash v1.0.3 // indirect
	github.com/sourcegraph/conc v0.3.0 // indirect
	github.com/spf13/afero v1.12.0 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/spf13/pflag v1.0.6 // indirect
	github.com/spf13/viper v1.20.1 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	go.uber.org/atomic v1.9.0 // indirect
	go.uber.org/multierr v1.9.0 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/tools v0.51.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.72.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Preserve Linux masquerade port-range flags; see the local fork notice.
replace github.com/google/nftables => ./third_party/google-nftables
