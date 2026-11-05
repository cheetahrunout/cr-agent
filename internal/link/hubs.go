package link

import (
	"errors"
	"net/url"
)

// ProductionHost is the only hub host a release build accepts (ruling B11).
const ProductionHost = "cr-ai.apps.grovestreet.zip"

// ValidateHubURL accepts exactly wss://cr-ai.apps.grovestreet.zip/agent; a debug build also accepts
// the local hosts in hubs_debug.go. No credentials, query or fragment are allowed.
func ValidateHubURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/agent" {
		return "", errors.New("hub URL must be <scheme>://<host>/agent with nothing else")
	}
	if u.Scheme == "wss" && u.Host == ProductionHost {
		return u.String(), nil
	}
	if debugHostAllowed(u) {
		return u.String(), nil
	}
	return "", errors.New("hub host not accepted by this build")
}

// FromPairingLink turns the link part of a pairing string (https://<host>/agent) into the hub's
// WebSocket URL, through the same host allowlist.
func FromPairingLink(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws" // only a debug build accepts a ws:// hub
	default:
		return "", errors.New("pairing link must start with https://")
	}
	return ValidateHubURL(u.String())
}

// PairEndpoint is the HTTP pairing URL of a validated hub URL: wss://host/agent → https://host/agent/pair.
func PairEndpoint(hubURL string) (string, error) {
	u, err := url.Parse(hubURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	default:
		return "", errors.New("not a hub URL")
	}
	u.Path = "/agent/pair"
	return u.String(), nil
}

// OpenURL is the page a browser opens for "Open hub": https://host/.
func OpenURL(hubURL string) string {
	u, err := url.Parse(hubURL)
	if err != nil {
		return ""
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
	} else {
		u.Scheme = "https"
	}
	u.Path = "/"
	return u.String()
}
