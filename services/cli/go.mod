module github.com/caelum0x/ration/services/cli

go 1.24

require (
	github.com/caelum0x/ration/pkg v0.0.0
	github.com/spf13/cobra v1.8.1
	google.golang.org/grpc v1.69.4
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	golang.org/x/net v0.33.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241223144023-3abc09e42ca8 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/caelum0x/ration/pkg => ../../pkg
