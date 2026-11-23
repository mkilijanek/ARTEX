//go:build unix

package traffic

import "syscall"

// noFollow makes an open fail when the last component is a link.
const noFollow = syscall.O_NOFOLLOW
