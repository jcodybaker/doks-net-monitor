package dnsecho

import "testing"

func TestDiffWithOverflow(t *testing.T) {
	tcs := []struct {
		name             string
		id               uint16
		lastID           uint16
		expectDiff       uint16
		expectOutOfOrder bool
	}{
		{
			name:             "simple happy-path",
			id:               1,
			lastID:           0,
			expectDiff:       1,
			expectOutOfOrder: false,
		},
		{
			name:             "simple happy-path + 1",
			id:               2,
			lastID:           0,
			expectDiff:       2,
			expectOutOfOrder: false,
		},
		{
			name:             "simple out-of-order",
			id:               0,
			lastID:           1,
			expectDiff:       1,
			expectOutOfOrder: true,
		},
		{
			name:             "overflow out-of-order",
			id:               65535,
			lastID:           0,
			expectDiff:       1,
			expectOutOfOrder: true,
		},
		{
			name:             "out-of-order top range",
			id:               65534,
			lastID:           65535,
			expectDiff:       1,
			expectOutOfOrder: true,
		},
		{
			name:             "simple overflow",
			id:               0,
			lastID:           65535,
			expectDiff:       1,
			expectOutOfOrder: false,
		},
		{
			name:             "simple overflow + 1",
			id:               1,
			lastID:           65535,
			expectDiff:       2,
			expectOutOfOrder: false,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			diff, outOfOrder := diffWithOverflow(tc.id, tc.lastID)
			if tc.expectOutOfOrder {
				if !outOfOrder {
					t.Errorf("expected outOfOrder %t, got %t", tc.expectOutOfOrder, outOfOrder)
				}
				return
			}
			if diff != tc.expectDiff {
				t.Errorf("expected diff %d, got %d", tc.expectDiff, diff)
			}
		})
	}
}
