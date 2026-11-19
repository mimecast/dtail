package common

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize parses a size string like "10MB", "1GB" into bytes
func ParseSize(sizeStr string) (int64, error) {
	originalStr := sizeStr
	sizeStr = strings.ToUpper(strings.TrimSpace(sizeStr))

	// Handle single-letter suffixes (K, M, G, T) by adding B
	if len(sizeStr) > 1 {
		lastChar := sizeStr[len(sizeStr)-1]
		secondLastChar := byte('0')
		if len(sizeStr) > 1 {
			secondLastChar = sizeStr[len(sizeStr)-2]
		}

		// If ends with K, M, G, or T and the character before it is a digit, add B
		if (lastChar == 'K' || lastChar == 'M' || lastChar == 'G' || lastChar == 'T') &&
			(secondLastChar >= '0' && secondLastChar <= '9') {
			sizeStr += "B"
		}
	}

	// Order matters - check longer suffixes first
	suffixes := []struct {
		suffix     string
		multiplier int64
	}{
		{"TB", 1024 * 1024 * 1024 * 1024},
		{"GB", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KB", 1024},
		{"B", 1},
	}

	for _, s := range suffixes {
		if strings.HasSuffix(sizeStr, s.suffix) {
			numStr := strings.TrimSuffix(sizeStr, s.suffix)
			numStr = strings.TrimSpace(numStr)
			if numStr == "" {
				return 0, fmt.Errorf("no number before size suffix")
			}
			num, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size number: %s (original: %s, processed: %s)", numStr, originalStr, sizeStr)
			}
			return int64(num * float64(s.multiplier)), nil
		}
	}

	// Try parsing as plain number (assume bytes)
	num, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size format: %s", sizeStr)
	}
	return num, nil
}

// FormatSize formats bytes into human-readable size
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
