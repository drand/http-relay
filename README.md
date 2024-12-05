## drand HTTP relay

This repo is a complete rewrite of the HTTP relays that could initially be found in drand/drand repo to be a self-sufficient binary that connects to a (set of) drand node(s) and queries them in the provided order in case of errors (i.e. the latter ones in the list are fallbacks in case the first one is down.)

This is also conveniently implemented using Chi and Chi middleware, allowing us to easily provide local caching, authentication, rate-limiting based on authentication, and other features in the future as needed.

# How to use the HTTP relays

Ideally a drand v2 node should only talk gRPC.
Therefore, this repo replaces the `--public-listen` flag from `drand` to allow to serve drand beacons over HTTP.

The relay connect to a drand node (or multiple) using gRPC, and then proceed to query beacons from these nodes to serve them
over HTTP. The relay are also in charge of properly setting the Cache-Control headers for beacons, to allow for optimal caching and 
CDN usage for serving beacons.

Typically usage, assuming you have a drand node running as:
```
drand start --folder /tmp/drand-full/node --control 33383 --private-listen 0.0.0.0:39107 --public-listen 0.0.0.0:35097 --verbose
```

You can replace the `--public-listen 0.0.0.0:35097` flag with this relay by running:
``` 
make
./drand-relay-http --grpc-connect "127.0.0.1:35097" --bind 0.0.0.0:35097 --verbose --metrics 127.0.0.1:9992
```

the `--verbose` and `--metrics` flags are optional, especially the `--verbose` one since it exposes DEBUG level gRPC logs. 

---

### License

This project is licensed using the [Permissive License Stack](https://protocol.ai/blog/announcing-the-permissive-license-stack/) which means that all contributions are available under the most permissive commonly-used licenses, and dependent projects can pick the license that best suits them.

Therefore, the project is dual-licensed under Apache 2.0 and MIT terms:

- Apache License, Version 2.0, ([LICENSE-APACHE](LICENSE-APACHE) or https://www.apache.org/licenses/LICENSE-2.0)
- MIT license ([LICENSE-MIT](LICENSE-MIT) or https://opensource.org/licenses/MIT)
89 
