// Module smooth-operator-core/go/temporal is the OPTIONAL Temporal-backed durable
// execution backend for the Go agent engine (ADR-030). It is a SEPARATE Go module
// on purpose — the mirror of the Rust `smooth-operator-temporal` crate's `temporal`
// cargo feature: the engine's default build (github.com/SmooAI/smooth-operator-core/go)
// pulls in NONE of the Temporal SDK and stays zero-infra. Only a consumer that
// imports THIS module takes on go.temporal.io/sdk.
module github.com/SmooAI/smooth-operator-core/go/temporal

go 1.26

require (
	github.com/SmooAI/smooth-operator-core/go v1.8.10
	go.temporal.io/sdk v1.36.0
)

require (
	github.com/BurntSushi/toml v1.4.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/facebookgo/clock v0.0.0-20150410010913-600d898af40a // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/mock v1.6.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/go-grpc-middleware/v2 v2.3.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.22.0 // indirect
	github.com/nexus-rpc/sdk-go v0.3.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/robfig/cron v1.2.0 // indirect
	github.com/stretchr/objx v0.5.2 // indirect
	github.com/stretchr/testify v1.10.0 // indirect
	go.temporal.io/api v1.51.0 // indirect
	golang.org/x/net v0.39.0 // indirect
	golang.org/x/sync v0.13.0 // indirect
	golang.org/x/sys v0.32.0 // indirect
	golang.org/x/text v0.24.0 // indirect
	golang.org/x/time v0.3.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240827150818-7e3bb234dfed // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240827150818-7e3bb234dfed // indirect
	google.golang.org/grpc v1.67.1 // indirect
	google.golang.org/protobuf v1.36.6 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Local dev / in-repo build: resolve the engine module from the sibling directory
// rather than the module proxy (mirrors the Rust crate's `path = "../smooth-operator-core"`).
replace github.com/SmooAI/smooth-operator-core/go => ../
