//go:build !windows

package main

import (
	"fmt"
	"os"
)

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func runWindow(url string) {
	fmt.Println("Open:", url)
	openBrowser(url)
	select {}
}
