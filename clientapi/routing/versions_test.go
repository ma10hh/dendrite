// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import "testing"

func TestUnstableFeatures(t *testing.T) {
	features := unstableFeatures(nil)
	for _, want := range []string{
		"org.matrix.e2e_cross_signing", "org.matrix.msc2285.stable", "org.matrix.msc3916.stable",
		"org.matrix.simplified_msc3575", // Element X will not use sliding sync without this
		"org.matrix.msc4222",            // /sync supports state_after
		"org.matrix.msc3030",            // timestamp_to_event
		"org.matrix.msc3266",            // room summary
	} {
		if !features[want] {
			t.Errorf("%s must always be advertised", want)
		}
	}

	features = unstableFeatures([]string{"msc2836"})
	if !features["org.matrix.msc2836"] || !features["org.matrix.simplified_msc3575"] {
		t.Errorf("configured MSCs are added to the fixed ones: %v", features)
	}
	if unstableFeatures(nil)["org.matrix.msc2836"] {
		t.Error("an MSC that is not configured must not be advertised")
	}
}
