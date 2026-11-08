module github.com/garamsh/mvmd/daemon

go 1.26.0

require (
	github.com/garamsh/mvmd/lib/runtimeapi v0.0.0
	github.com/stretchr/testify v1.12.1
	go.etcd.io/bbolt v1.5.0
	golang.org/x/sys v0.49.0
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)

require (
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
)

replace github.com/garamsh/mvmd/lib/runtimeapi => ../lib/runtimeapi
