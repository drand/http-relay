## drand HTTP relay

This repo is a complete rewrite of the HTTP relays that could initially be found in drand/drand and later in drand/drand-cli to be a self-sufficient binary that connects to a (set of) drand node(s) and queries them in the provided order in case of errors. (i.e. the latter ones in the list are fallbacks in case the first one is down.)

This is also conveniently implemented using Chi and Chi middleware, allowing us to easily provide local caching, authentication, rate-limiting based on authentication, and other features in the future as needed.

This is a minimal dependency project, meant to run without CGO but also with CGO.

# Benchmarks

When requests are done on time:

When requests are done 20ms before the round is supposed to be emitted:

When requests are done 20ms after the round is supposed to be emitted:

When requests are done 700ms after the round is supposed to be emitted:

When requests are done 800ms after the round is supposed to be emitted:


---

### License

This project is licensed using the [Permissive License Stack](https://protocol.ai/blog/announcing-the-permissive-license-stack/) which means that all contributions are available under the most permissive commonly-used licenses, and dependent projects can pick the license that best suits them.

Therefore, the project is dual-licensed under Apache 2.0 and MIT terms:

- Apache License, Version 2.0, ([LICENSE-APACHE](LICENSE-APACHE) or https://www.apache.org/licenses/LICENSE-2.0)
- MIT license ([LICENSE-MIT](LICENSE-MIT) or https://opensource.org/licenses/MIT)
89 
