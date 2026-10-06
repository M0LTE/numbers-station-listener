// Package data embeds the station catalogue so the binary is self-contained.
// The catalogue derives from Priyom.org and is CC BY-NC-SA 4.0; see README.md.
package data

import _ "embed"

//go:embed stations.json
var StationsJSON []byte
