package utils

import (
	"encoding/json"
	"fmt"
	"math/big"
)

func Average(values []*big.Int) *big.Int {
	total := big.NewInt(0)
	for _, v := range values {
		total.Add(total, v)
	}
	return big.NewInt(0).Div(total, big.NewInt(int64(len(values))))
}

func Sum(values []*big.Int) *big.Int {
	sum := big.NewInt(0)
	for _, v := range values {
		sum.Add(sum, v)
	}
	return sum
}

// BigInt is a wrapper for big.Int to implement custom JSON marshaling/unmarshaling.
type BigInt struct {
	Int *big.Int `json:"int"`
}

// MarshalJSON marshals the BigInt into JSON, converting it to a string.
func (b BigInt) MarshalJSON() ([]byte, error) {
	return json.Marshal(b.Int.String()) // Convert to string
}

// UnmarshalJSON unmarshals a JSON string into BigInt.
func (b *BigInt) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	intValue, success := new(big.Int).SetString(str, 10) // Parse from base 10 string
	if !success {
		return fmt.Errorf("failed to unmarshal BigInt: %s", str)
	}
	b.Int = intValue
	return nil
}
