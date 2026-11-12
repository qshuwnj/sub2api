package service

import (
	"reflect"
	"strings"
)

// EqualForPersistence is deliberately conservative: a false result may trigger
// a full write, but must never hide a change to the exact native-video price.
func (p ChannelModelPricing) EqualForPersistence(other ChannelModelPricing) bool {
	if !reflect.DeepEqual(p, other) {
		return false
	}
	for i, interval := range p.Intervals {
		if !strings.HasPrefix(interval.TierLabel, "video:") {
			continue
		}
		// DeepEqual compares pointed-to values, not alias relationships. Replacing
		// a float pointer invalidates its captured decimal even for equal floats.
		beforeCaptured := interval.perRequestPriceSource == interval.PerRequestPrice
		after := other.Intervals[i]
		afterCaptured := after.perRequestPriceSource == after.PerRequestPrice
		if beforeCaptured != afterCaptured {
			return false
		}
	}
	return true
}
