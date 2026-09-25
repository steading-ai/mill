package cache

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/steading-ai/mill/internal/workspace/machine/driver"
)

// Watermark is an amount of free space on the sandbox filesystem: a share
// of its size, or a number of bytes (docs/sandbox.md §Eviction). Eviction
// starts when free space is below the high one and stops once it is above
// the low one, so the low watermark is the larger amount of free space:
// the names say how full the volume is, the numbers say what is free.
type Watermark struct {
	// Percent is the share of the filesystem's size, when positive.
	Percent float64
	// Bytes is the amount, when Percent is zero.
	Bytes uint64
}

// ParseWatermark reads `10%`, or a size such as `50G` or a bare number of
// bytes.
func ParseWatermark(text string) (Watermark, error) {
	text = strings.TrimSpace(text)
	if share, ok := strings.CutSuffix(text, "%"); ok {
		percent, err := strconv.ParseFloat(strings.TrimSpace(share), 64)
		if err != nil || percent <= 0 || percent > 100 {
			return Watermark{}, fmt.Errorf("%q is not a watermark: a percentage of the filesystem such as 10%%, or a size such as 50G", text)
		}
		return Watermark{Percent: percent}, nil
	}
	bytes, err := ParseSize(text)
	if err != nil || bytes == 0 {
		return Watermark{}, fmt.Errorf("%q is not a watermark: a percentage of the filesystem such as 10%%, or a size such as 50G", text)
	}
	return Watermark{Bytes: bytes}, nil
}

// IsZero reports a watermark nothing set.
func (w Watermark) IsZero() bool { return w.Percent == 0 && w.Bytes == 0 }

// Of is the watermark in bytes on a filesystem.
func (w Watermark) Of(fs driver.Filesystem) uint64 {
	if w.Percent > 0 {
		return uint64(float64(fs.Total) * w.Percent / 100)
	}
	return w.Bytes
}

// String is how the watermark was spelled: `10%` or `50G`.
func (w Watermark) String() string {
	if w.Percent > 0 {
		return strconv.FormatFloat(w.Percent, 'f', -1, 64) + "%"
	}
	return FormatSize(w.Bytes)
}

// FormatSize spells bytes the way `du -h` would: the largest binary unit
// that keeps the number above one, one decimal.
func FormatSize(bytes uint64) string {
	units := []string{"B", "K", "M", "G", "T"}
	value := float64(bytes)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d%s", bytes, units[unit])
	}
	if value == float64(uint64(value)) {
		return fmt.Sprintf("%d%s", uint64(value), units[unit])
	}
	return fmt.Sprintf("%.1f%s", value, units[unit])
}

// ParseSize reads a size such as `20G`, `512M`, `1.5T` or a bare number of
// bytes, in binary units.
func ParseSize(text string) (uint64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("%q is not a size", text)
	}
	number := strings.TrimRight(text, "BKMGTbkmgt")
	suffix := strings.ToUpper(text[len(number):])
	suffix = strings.TrimSuffix(suffix, "B")
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%q is not a size", text)
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}
	n, ok := shift[suffix]
	if !ok {
		return 0, fmt.Errorf("%q is not a size", text)
	}
	return uint64(value * float64(uint64(1)<<n)), nil
}
