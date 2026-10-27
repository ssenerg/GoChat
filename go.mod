module github.com/ssenerg/gochat

go 1.27.3

require (
	github.com/anthdm/hollywood v1.0.5
	github.com/casbin/casbin/v2 v2.135.0
	github.com/coder/websocket v1.8.15
	github.com/go-viper/mapstructure/v2 v2.5.0
	github.com/gocql/gocql v1.7.0
	github.com/kljensen/snowball v0.10.0
	github.com/minio/minio-go/v7 v7.4.1
	github.com/pion/ice/v4 v4.4.8
	github.com/pion/interceptor v0.1.49
	github.com/pion/rtcp v1.2.20
	github.com/pion/sdp/v3 v3.0.20
	github.com/pion/webrtc/v4 v4.2.25
	github.com/pquerna/otp v1.5.0
	github.com/prometheus/client_golang v1.25.0
	github.com/scylladb/gocqlx/v3 v3.0.4
	github.com/spf13/cobra v1.10.2
	github.com/spf13/viper v1.21.0
	github.com/yuin/goldmark v1.8.6
	go.uber.org/zap v1.28.0
	golang.org/x/crypto v0.58.0
	golang.org/x/image v0.47.0
	golang.org/x/net v0.60.0
	golang.org/x/sync v0.24.0
	golang.org/x/text v0.43.0
	golang.org/x/time v0.16.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
	storj.io/drpc v1.0.0
)

require (
	github.com/DataDog/gostackparse v0.7.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bmatcuk/doublestar/v4 v4.6.1 // indirect
	github.com/boombuler/barcode v1.0.1-0.20190219062509-6c824513bacc // indirect
	github.com/casbin/govaluate v1.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/klauspost/compress v1.20.1 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pion/datachannel v1.6.3 // indirect
	github.com/pion/dtls/v3 v3.1.15 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.2.2 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtp v1.10.6 // indirect
	github.com/pion/sctp v1.12.0 // indirect
	github.com/pion/srtp/v3 v3.1.3 // indirect
	github.com/pion/stun/v4 v4.0.1 // indirect
	github.com/pion/transport/v5 v5.1.1 // indirect
	github.com/pion/turn/v5 v5.1.2 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/scylladb/go-reflectx v1.0.1 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/zeebo/errs v1.2.2 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.49.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/ini.v1 v1.67.3 // indirect
)

tool (
	google.golang.org/protobuf/cmd/protoc-gen-go
	storj.io/drpc/cmd/protoc-gen-go-drpc
)

replace github.com/gocql/gocql => github.com/scylladb/gocql v1.20.0
