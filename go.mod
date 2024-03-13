module github.com/drand/http-server

go 1.21

require (
	github.com/drand/drand v1.5.11
	github.com/go-chi/chi/v5 v5.0.12
	github.com/go-chi/httplog/v2 v2.0.9
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/stretchr/testify v1.9.0
	google.golang.org/grpc v1.62.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/net v0.22.0 // indirect
	golang.org/x/sys v0.18.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240311173647-c811ad7063a7 // indirect
	google.golang.org/protobuf v1.33.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/drand/drand => ../drand
