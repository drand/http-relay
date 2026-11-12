drand-relay-http:
	go build -o drand-relay-http

bench:
	go test -run '^$$' -bench . -benchtime 10x -benchmem ./...
