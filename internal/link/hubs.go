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
