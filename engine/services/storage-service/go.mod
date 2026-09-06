module github.com/codeaudit/services/storage-service

go 1.22

require (
	github.com/codeaudit/go-config v0.0.0
	github.com/codeaudit/proto-gen v1.0.0
	github.com/minio/minio-go/v7 v7.0.74
	github.com/redis/go-redis/v9 v9.5.3
	github.com/segmentio/kafka-go v0.4.47
	google.golang.org/grpc v1.65.0
	google.golang.org/protobuf v1.34.2
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/goccy/go-json v0.10.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/klauspost/cpuid/v2 v2.2.8 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	github.com/rs/xid v1.5.0 // indirect
	golang.org/x/crypto v0.24.0 // indirect
	golang.org/x/net v0.26.0 // indirect
	golang.org/x/sys v0.21.0 // indirect
	golang.org/x/text v0.16.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240528184218-531527333157 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/codeaudit/proto-gen => ../../libs/proto-gen/go

replace github.com/codeaudit/go-config => ../../libs/go-config
