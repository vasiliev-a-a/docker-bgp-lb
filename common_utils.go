package main

import (
	"crypto/md5"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
)

func generateMacAddressFromID(macAddressID string) string {
	// Generate an hash from the previous string and truncate it to 6 bytes (48 bits = MAC Length).
	hasher := md5.New()
	hasher.Write([]byte(macAddressID))
	macAddressBytes := hasher.Sum(nil)[:6]

	// Convert the byte array into an hex encoded string separated by `:`.
	// This will be the MAC Address of the interface.
	macAddressString := []string{}

	for _, element := range macAddressBytes {
		macAddressString = append(macAddressString, fmt.Sprintf("%02x", element))
	}

	// Steps to obtain a locally administered unicast MAC.
	// See http://www.noah.org/wiki/MAC_address.
	firstByteInt, _ := strconv.ParseInt(macAddressString[0], 16, 32)
	macAddressString[0] = fmt.Sprintf("%02x", (firstByteInt|0x02)&0xfe)

	return strings.Join(macAddressString, ":")
}

// newBackoff returns an exponential backoff where the context is the only
// limiting factor (MaxElapsedTime is disabled).
func newBackoff(initial time.Duration, multiplier, randomization float64, max time.Duration) *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(initial),
		backoff.WithMultiplier(multiplier),
		backoff.WithRandomizationFactor(randomization),
		backoff.WithMaxInterval(max),
		backoff.WithMaxElapsedTime(0),
	)
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}

	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}

	// If we reach here, it means the string is shorter than n, so we return the original string.
	return s
}
