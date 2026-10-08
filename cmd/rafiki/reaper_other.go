//go:build !linux

package main

func reapAsInit() (int, bool) { return 0, false }
