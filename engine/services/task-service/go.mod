module github.com/codeaudit/services/task-service

go 1.22

require (
	github.com/codeaudit/go-config v0.0.0
	github.com/codeaudit/proto-gen v0.0.0
	google.golang.org/grpc v1.65.0
	google.golang.org/protobuf v1.34.2
)

require (
	github.com/klauspost/compress v1.15.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	github.com/segmentio/kafka-go v0.4.47 // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	golang.org/x/text v0.15.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240528184218-531527333157 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/codeaudit/proto-gen => ../../libs/proto-gen/go

replace github.com/codeaudit/go-config => ../../libs/go-config
