package host

import "testing"

func TestAverageClockKHz(t *testing.T) {
	got := averageClockKHz([]string{"2808000\n", "3900000", "offline", "0"})
	if got != 3354 {
		t.Fatalf("average clock = %.0f MHz, want 3354", got)
	}
}

func TestParseKernelVersion(t *testing.T) {
	for input, want := range map[string]string{
		"6.11.0-1016-nvidia\n": "6.11.0-1016-nvidia",
		"6.8.0-85-generic":     "6.8.0-85-generic",
		"":                     "",
		"6.8 bad\"label":       "",
		"-leading-dash":        "",
	} {
		if got := parseKernelVersion(input); got != want {
			t.Errorf("parseKernelVersion(%q) = %q, want %q", input, got, want)
		}
	}
}
