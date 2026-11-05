// Package releasekeys embeds release-keys.json, the list of public keys that may sign cr-agent
// releases (ruling B20). The agent and the launcher compile it in; the hub reads the same file from
// its submodule.
package releasekeys

import _ "embed"

// JSON is release-keys.json.
//
//go:embed release-keys.json
var JSON []byte
