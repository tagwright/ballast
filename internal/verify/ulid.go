// SPDX-License-Identifier: GPL-3.0-or-later

package verify

import "github.com/tagwright/ballast/internal/ulid"

// newULID returns a fresh ULID for a verify_id. ULID generation only fails if
// the system CSPRNG does, which is unrecoverable everywhere else it is used; on
// that impossible path a fixed sentinel keeps the record schema-valid (26
// Crockford characters) rather than producing an empty id.
func newULID() string {
	id, err := ulid.New()
	if err != nil {
		return "00000000000000000000000000"
	}
	return id
}
