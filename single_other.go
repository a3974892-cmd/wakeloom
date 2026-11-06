//go:build !windows

package main

import "net"

var singleLn net.Listener

func ensureSingleInstance() {
	ln, err := net.Listen("tcp", "127.0.0.1:47819")
	if err != nil {
		fatal("Wakeloom is already running")
	}
	singleLn = ln
}
