package reconcile

import "testing"

// TestPriorityFromLabels: a flowbee:p<N> label sets the intake priority; absence (or a
// malformed label) returns 0, which the store normalizes to the default 5.
func TestPriorityFromLabels(t *testing.T) {
	cases := []struct {
		labels []string
		want   int
	}{
		{[]string{"flowbee:build"}, 0},                 // no priority label -> 0 (store defaults to 5)
		{[]string{"flowbee:build", "flowbee:p1"}, 1},   // urgent
		{[]string{"flowbee:p10", "flowbee:build"}, 10}, // nice-to-have
		{[]string{"flowbee:p3"}, 3},                    // arbitrary
		{[]string{"flowbee:pHIGH"}, 0},                 // malformed -> 0
		{[]string{"bug", "enhancement"}, 0},            // unrelated labels
		{nil, 0},                                       // no labels
		{[]string{"flowbee:p99"}, 99},                  // raw value; store NormalizePriority clamps to 10
	}
	for _, c := range cases {
		if got := priorityFromLabels(c.labels); got != c.want {
			t.Errorf("priorityFromLabels(%v) = %d, want %d", c.labels, got, c.want)
		}
	}
}
