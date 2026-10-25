// Package usagepage embeds the usage dashboard served at /usage.html.
package usagepage

import _ "embed"

// HTML is the self-contained usage dashboard page.
//
//go:embed usage.html
var HTML []byte
