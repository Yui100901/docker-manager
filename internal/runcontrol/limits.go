package runcontrol

import (
	"fmt"
	"math"
	"time"
)

const (
	MaxConcurrency = 64
	MaxTimeout     = 24 * time.Hour
	MaxRate        = 1000.0
	MaxItems       = 100_000
)

// Limits bounds one command's scalable read and inspection work. Zero values
// leave the corresponding control disabled so existing commands can opt in
// without changing their defaults.
type Limits struct {
	Concurrency int
	Timeout     time.Duration
	Rate        float64
	MaxItems    int
}

func Validate(limits Limits) error {
	return limits.Validate()
}

func (limits Limits) Validate() error {
	if limits.Concurrency < 0 || limits.Concurrency > MaxConcurrency {
		return fmt.Errorf("concurrency must be between 0 and %d", MaxConcurrency)
	}
	if limits.Timeout < 0 || limits.Timeout > MaxTimeout {
		return fmt.Errorf("operation timeout must be between 0 and %s", MaxTimeout)
	}
	if math.IsNaN(limits.Rate) || math.IsInf(limits.Rate, 0) || limits.Rate < 0 || limits.Rate > MaxRate {
		return fmt.Errorf("rate limit must be between 0 and %g", MaxRate)
	}
	if limits.MaxItems < 0 || limits.MaxItems > MaxItems {
		return fmt.Errorf("max items must be between 0 and %d", MaxItems)
	}
	return nil
}
