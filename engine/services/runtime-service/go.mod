module github.com/codeaudit/services/runtime-service

go 1.22

require (
	github.com/codeaudit/common-go v0.0.0
	github.com/codeaudit/go-config v0.0.0
	github.com/codeaudit/proto-gen v0.0.0
	github.com/lib/pq v1.12.3
	github.com/redis/go-redis/v9 v9.5.1
	golang.org/x/text v0.15.0
	google.golang.org/grpc v1.65.0
	google.golang.org/protobuf v1.34.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240528184218-531527333157 // indirect
)

replace github.com/codeaudit/proto-gen => ../../libs/proto-gen/go

replace github.com/codeaudit/common-go => ../../libs/common-go

replace github.com/codeaudit/go-config => ../../libs/go-config
