//go:build unix

package api

import "syscall"

const openNoFollowNonblock = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
