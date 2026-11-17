// SPDX-License-Identifier: AGPL-3.0-or-later

package playertest

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
