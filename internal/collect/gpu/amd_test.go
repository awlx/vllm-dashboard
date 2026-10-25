package gpu

import (
	"math"
	"testing"
)

// Fixtures follow the amd-smi metric JSON layout of each ROCm release.
const amdSMIROCm7MultiGPU = `{
    "gpu_data": [
        {
            "gpu": 0,
            "usage": {"gfx_activity": {"value": 87, "unit": "%"}, "umc_activity": {"value": 40, "unit": "%"}, "mm_activity": "N/A", "vcn_activity": ["N/A", "N/A", "N/A", "N/A"], "jpeg_activity": ["N/A"], "gfx_busy_inst": "N/A", "jpeg_busy": "N/A", "vcn_busy": "N/A"},
            "power": {"socket_power": {"value": 612, "unit": "W"}, "gfx_voltage": "N/A", "soc_voltage": "N/A", "mem_voltage": "N/A", "throttle_status": "UNTHROTTLED", "power_management": "ENABLED"},
            "clock": {"gfx_0": {"clk": {"value": 2100, "unit": "MHz"}, "min_clk": {"value": 500, "unit": "MHz"}, "max_clk": {"value": 2100, "unit": "MHz"}, "clk_locked": "DISABLED", "deep_sleep": "DISABLED"}, "gfx_1": {"clk": "N/A", "min_clk": "N/A", "max_clk": "N/A", "clk_locked": "N/A", "deep_sleep": "N/A"}, "mem_0": {"clk": {"value": 1300, "unit": "MHz"}, "min_clk": "N/A", "max_clk": "N/A", "clk_locked": "N/A", "deep_sleep": "N/A"}},
            "temperature": {"edge": "N/A", "hotspot": {"value": 71, "unit": "C"}, "mem": {"value": 60, "unit": "C"}}
        },
        {
            "gpu": 1,
            "usage": {"gfx_activity": {"value": 13, "unit": "%"}, "umc_activity": {"value": 2, "unit": "%"}, "mm_activity": "N/A"},
            "power": {"socket_power": "N/A", "gfx_voltage": "N/A", "soc_voltage": "N/A", "mem_voltage": "N/A", "throttle_status": "N/A", "power_management": "ENABLED"},
            "clock": {"gfx_0": {"clk": {"value": 1900, "unit": "MHz"}, "min_clk": "N/A", "max_clk": "N/A", "clk_locked": "N/A", "deep_sleep": "N/A"}},
            "temperature": {"edge": "N/A", "hotspot": {"value": 49, "unit": "C"}, "mem": {"value": 45, "unit": "C"}}
        }
    ]
}`

const amdSMIROCm6Array = `[
    {
        "gpu": 0,
        "usage": {"gfx_activity": {"value": 50, "unit": "%"}, "umc_activity": {"value": 10, "unit": "%"}, "mm_activity": "N/A"},
        "power": {"socket_power": {"value": 200, "unit": "W"}, "gfx_voltage": {"value": 900, "unit": "mV"}, "soc_voltage": "N/A", "mem_voltage": "N/A", "power_management": "ENABLED", "throttle_status": "UNTHROTTLED"},
        "clock": {"gfx_0": {"clk": {"value": 1800, "unit": "MHz"}, "min_clk": {"value": 500, "unit": "MHz"}, "max_clk": {"value": 2500, "unit": "MHz"}, "clk_locked": "DISABLED", "deep_sleep": "DISABLED"}},
        "temperature": {"edge": {"value": 55, "unit": "C"}, "hotspot": {"value": 65, "unit": "C"}, "mem": {"value": 60, "unit": "C"}}
    }
]`

const amdSMIROCm61Array = `[
    {
        "gpu": 0,
        "usage": {"gfx_activity": {"value": 30, "unit": "%"}, "umc_activity": {"value": 5, "unit": "%"}, "mm_activity": "N/A"},
        "power": {"socket_power": {"value": 150, "unit": "W"}, "gfx_voltage": "N/A", "soc_voltage": "N/A", "mem_voltage": "N/A"},
        "clock": {"gfx": {"clk": {"value": 1500, "unit": "MHz"}, "min_clk": {"value": 500, "unit": "MHz"}, "max_clk": {"value": 2100, "unit": "MHz"}, "sleep_clk": "N/A"}},
        "temperature": {"edge": {"value": 40, "unit": "C"}, "hotspot": {"value": 45, "unit": "C"}, "mem": {"value": 42, "unit": "C"}}
    }
]`

const amdSMIROCm60Array = `[
    {
        "gpu": 0,
        "usage": {"gfx_usage": 25, "mem_usage": 3, "mm_ip_usage": "N/A"},
        "power": {"current_power": 120, "current_gfx_voltage": "N/A", "power_limit": 300},
        "clock": {"gfx": {"cur_clk": 1400, "min_clk": 500, "max_clk": 2100}},
        "temperature": {"edge": 35, "hotspot": 38, "mem": 33}
    }
]`

const amdSMIAllUnavailable = `{"gpu_data": [
    {"gpu": 0, "usage": "N/A",
     "power": {"socket_power": "N/A"},
     "clock": {"gfx_0": {"clk": "N/A"}},
     "temperature": {"edge": "N/A", "hotspot": "N/A", "mem": "N/A"}}
]}`

func TestParseAMDSMIMetrics(t *testing.T) {
	cases := []struct {
		name                     string
		in                       string
		util, temp, power, clock float64
	}{
		{"rocm7 wrapper with N/A and hotspot fallback", amdSMIROCm7MultiGPU, 50, 60, 612, 2000},
		{"rocm6.2+ top-level array", amdSMIROCm6Array, 50, 55, 200, 1800},
		{"rocm6.1 gfx clock key", amdSMIROCm61Array, 30, 40, 150, 1500},
		{"rocm6.0 plain numbers and old keys", amdSMIROCm60Array, 25, 35, 120, 1400},
		{"numeric strings", `[{"usage": {"gfx_activity": {"value": "12.5"}}, "temperature": {"junction": "70"}, "power": {"socket_power": " 80 "}, "clock": {"gfx_0": {"clk": "1000"}}}]`, 12.5, 70, 80, 1000},
		{"all values unavailable", amdSMIAllUnavailable, 0, 0, 0, 0},
		{"NaN string is ignored", `[{"usage": {"gfx_activity": "NaN"}}]`, 0, 0, 0, 0},
		{"empty gpu list", `{"gpu_data": []}`, 0, 0, 0, 0},
		{"unknown wrapper", `{"cpu_data": []}`, 0, 0, 0, 0},
		{"invalid JSON", `not json`, 0, 0, 0, 0},
		{"empty output", ``, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			util, temp, power, clock := parseAMDSMIMetrics([]byte(tc.in))
			got := []float64{util, temp, power, clock}
			want := []float64{tc.util, tc.temp, tc.power, tc.clock}
			for i, name := range []string{"util", "temp", "power", "clock"} {
				if math.Abs(got[i]-want[i]) > 1e-9 {
					t.Errorf("%s = %v, want %v", name, got[i], want[i])
				}
			}
		})
	}
}
