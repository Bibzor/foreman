package sandbox

// SetUID makes Read see uid as the current user, until the returned func
// restores it.
func SetUID(uid int) (restore func()) {
	old := getuid
	getuid = func() int { return uid }
	return func() { getuid = old }
}
