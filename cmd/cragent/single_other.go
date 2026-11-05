//go:build !windows

package main

func singleInstance(string) (release func(), running bool) { return func() {}, false }
