// Package accounts owns API-key account schemas, stable identity, legacy
// fallback reads, atomic pool persistence, and cross-process mutation locking.
//
// Pool storage has two backends (the Backend enum): file keeps secret values
// inline in the pool JSON (historical layout); keychain stores secret values
// in the OS keychain via internal/credstore while the pool file carries
// metadata only (see store_keychain.go). The backend is selected by config
// `credentials:` and resolved through NewStore's process default.
//
// It deliberately has no dependency on application config, providers, login
// validation, routing, reload, Web, or CLI orchestration.
package accounts
