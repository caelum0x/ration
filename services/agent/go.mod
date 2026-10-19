module github.com/caelum0x/ration/services/agent

go 1.24.4

require (
	github.com/caelum0x/ration/pkg v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.69.4
)

require (
	golang.org/x/net v0.33.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241223144023-3abc09e42ca8 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/caelum0x/ration/pkg => ../../pkg
