//go:build !unix

package api

// No O_NOFOLLOW/O_NONBLOCK here: the Lstat and SameFile checks of
// readDeployKey are the only guard.
const openNoFollowNonblock = 0
