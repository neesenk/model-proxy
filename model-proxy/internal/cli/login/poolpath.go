package login

// PoolPath resolves the plural credential pool file for a provider name.
func PoolPath(name string) string {
	return accountStoreEnv().PoolPath(name)
}
