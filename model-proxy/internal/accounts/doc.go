// Package accounts owns API-key account schemas, stable identity, legacy
// fallback reads, atomic pool persistence, and cross-process mutation locking.
//
// It deliberately has no dependency on application config, providers, login
// validation, routing, reload, Web, or CLI orchestration.
package accounts
