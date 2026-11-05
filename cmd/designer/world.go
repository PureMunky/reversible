package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
)

// The demo world. Customers differ by tenure, and how much they will pay
// depends on it. Nothing in the system is told any of this; it only sees
// each customer's inputs and, afterwards, whether they stayed.
//
//	segment   share   tenure (months)   leaves above
//	new        35%        0–5            $35–54
//	mid        40%        6–23           $45–74
//	loyal      25%       24–60           $60–99
//
// A single price has to trade the segments off against each other; the
// best one earns about $37 per customer. Pricing each segment at the
// bottom of its range earns about $45.

type customer struct {
	ID        string
	Tenure    int
	Seats     int // irrelevant to willingness to pay: a distractor
	Threshold int
}

func customerAt(i int) customer {
	id := fmt.Sprintf("c%06d", i)
	h := sha256.Sum256([]byte("designer-world:" + id))
	r := int(h[0]) % 100
	t := int(binary.BigEndian.Uint16(h[2:4]))
	c := customer{ID: id, Seats: 1 + int(h[4])%50}
	switch {
	case r < 35:
		c.Tenure, c.Threshold = int(h[1])%6, 35+t%20
	case r < 75:
		c.Tenure, c.Threshold = 6+int(h[1])%18, 45+t%30
	default:
		c.Tenure, c.Threshold = 24+int(h[1])%37, 60+t%40
	}
	return c
}

func (c customer) input() map[string]string {
	return map[string]string{"customer": c.ID, "tenure_months": strconv.Itoa(c.Tenure), "seats": strconv.Itoa(c.Seats)}
}

// outcome is what really happens when a customer is charged a price.
func (c customer) outcome(price int) map[string]float64 {
	if price > c.Threshold {
		return map[string]float64{"revenue": 0, "churn": 1}
	}
	return map[string]float64{"revenue": float64(price), "churn": 0}
}

// trueValue is the expected revenue per customer of a pricing function,
// measured over a large fixed population the demo never serves.
func trueValue(price func(customer) int) float64 {
	const n = 40000
	total := 0.0
	for i := 1_000_000; i < 1_000_000+n; i++ {
		c := customerAt(i)
		total += c.outcome(price(c))["revenue"]
	}
	return total / n
}
