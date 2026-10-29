package service

import (
	"encoding/json"
	"strings"
)

// Preserve native-video retail decimals through group JSON without changing the
// numeric wire shape or legacy image/audio pricing fields.
func (iv *PricingInterval) UnmarshalJSON(data []byte) error {
	type intervalJSON PricingInterval
	var decoded intervalJSON
	wire := struct {
		*intervalJSON
		Price json.RawMessage `json:"per_request_price"`
	}{intervalJSON: &decoded}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(wire.Price) != 0 {
		if err := json.Unmarshal(wire.Price, &decoded.PerRequestPrice); err != nil {
			return err
		}
	}
	value := PricingInterval(decoded)
	if strings.HasPrefix(value.TierLabel, "video:") {
		value.perRequestPriceDecimal = string(wire.Price)
		value.perRequestPriceSource = value.PerRequestPrice
		if _, err := videoIntervalPrice(value); err != nil {
			return err
		}
	}
	*iv = value
	return nil
}

func (iv PricingInterval) MarshalJSON() ([]byte, error) {
	type intervalJSON PricingInterval
	if !strings.HasPrefix(iv.TierLabel, "video:") {
		return json.Marshal(intervalJSON(iv))
	}
	price, err := videoIntervalPrice(iv)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		intervalJSON
		Price json.RawMessage `json:"per_request_price"`
	}{intervalJSON: intervalJSON(iv), Price: json.RawMessage(price.String())})
}
