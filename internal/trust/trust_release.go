//go:build !debug

package trust

// debugTrust is false in every build that can be released: only the production list is trusted.
const debugTrust = false
