package login

// poolPath resolves the plural credential pool file for a provider name.
func poolPath(name string) string {
	return accountStoreEnv().PoolPath(name)
}
