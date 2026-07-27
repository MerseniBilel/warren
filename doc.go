// Package warren is a DDD-first application framework for Go backends and
// microservices.
//
// Warren composes three ideas that are usually left to convention:
//
//   - A transport-agnostic unit of work. You write a use case once as a
//     [Handler]; HTTP, gRPC, CLI, and message consumers are thin adapters over
//     it. The use case imports no transport package.
//   - Domain-driven design as real types rather than folder names. Aggregate
//     roots, domain events, repositories, and the unit of work are framework
//     primitives.
//   - Architecture enforced in CI. The dependency rule between layers is
//     configuration, and violating it fails the build.
//
// Warren is not a web framework, an ORM, or a deployment platform. It composes
// existing routers and drivers behind stable ports.
//
// # Layout
//
// This repository is a multi-module workspace. The root module holds the
// framework core and carries no third-party dependencies. Transports, brokers,
// persistence drivers, and observability each live in their own module so that
// a service pays only for what it imports.
//
// See docs/architecture.md for the module graph and the layering rules, and
// docs/adr for the decisions behind them.
package warren
