//go:build debug

package main

// debugBuild marks a build that is never signed for release: it accepts a local hub, a local
// stand-in for Hugging Face (--hf-base) and scripted download approvals (--approve).
const debugBuild = true
