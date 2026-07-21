//go:build unix

package main

import "syscall"

func setSecureFileCreationMask() {
	syscall.Umask(0077)
}
