//go:build !windows

package samizdatreverse

import "syscall"

var (
	refusedErrnos      = []syscall.Errno{syscall.ECONNREFUSED}
	unreachableErrnos  = []syscall.Errno{syscall.ENETUNREACH, syscall.EHOSTUNREACH}
	resetErrnos        = []syscall.Errno{syscall.ECONNRESET, syscall.EPIPE}
	notPermittedErrnos = []syscall.Errno{syscall.EPERM, syscall.EACCES}
)
