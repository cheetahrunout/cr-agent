//go:build !debug

package main

// debugBuild is false in every build that can be released: --approve and --hf-base are refused.
const debugBuild = false
