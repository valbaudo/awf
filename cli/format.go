package cli

import "fmt"

// formatUSD renders a USD cost at a fixed 4-decimal precision so every display
// surface (ls, run summary, inspect) agrees on the same run's cost.
func formatUSD(usd float64) string { return fmt.Sprintf("$%.4f", usd) }
