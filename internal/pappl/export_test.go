package pappl

import "time"

// SetNow sets the clock c reads for the Find-Devices backoff.
func SetNow(c *Client, now func() time.Time) { c.now = now }
